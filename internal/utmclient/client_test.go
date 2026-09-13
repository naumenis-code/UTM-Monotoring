package utmclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// TestFetchInfoPrimaryAPIPath exercises the confirmed-working fast path
// (GET /api/info/list + GET /api/rsa) that a real УТМ was seen answering
// synchronously and locally. This is expected to be the common case in
// practice; fixture values below are fictitious (structurally matching the
// confirmed real shape, but not real organization data). The fixture
// includes several rows for the same owner (as a real device does, one per
// licensed address) to verify only the matching row is used.
func TestFetchInfoPrimaryAPIPath(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/api/info/list", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"4.2.0","contour":"prod","rsaError":null,"checkInfo":null,
			"ownerId":"030000000001",
			"rsa":{"certType":"RSA","startDate":"2030-01-01 00:00:00 +0000",
				"expireDate":"2031-01-01 00:00:00 +0000","isValid":"valid",
				"issuer":"pki.fsrar.ru","keyStartDate":null,"keyExpireDate":null,"isKeyValid":null},
			"gost":{"certType":"GOST","startDate":"2030-02-01 00:00:00 +0000",
				"expireDate":"2031-02-01 00:00:00 +0000","isValid":"valid",
				"issuer":"Пример УЦ","keyStartDate":"2030-02-01 00:00:00 +0000",
				"keyExpireDate":"2031-02-01 00:00:00 +0000","isKeyValid":"valid"},
			"license":false}`)
	})
	mux.HandleFunc("/api/rsa", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rows":[
			{"pass_owner_id":"030000000002","Owner_ID":"030000000002","Full_Name":"ИП ПЕТРОВ ПЁТР ПЕТРОВИЧ",
				"Short_Name":"ИП ПЕТРОВ ПЁТР ПЕТРОВИЧ","INN":"9876543210","KPP":"",
				"Dejure_Address":"г. Тверь, ул. Другая, д. 5","Fact_Address":"г. Тверь, ул. Другая, д. 5"},
			{"pass_owner_id":"030000000001","Owner_ID":"030000000001","Full_Name":"ИП ИВАНОВ ИВАН ИВАНОВИЧ",
				"Short_Name":"ИП ИВАНОВ ИВАН ИВАНОВИЧ","INN":"1234567890","KPP":"",
				"Dejure_Address":"г. Москва, ул. Примерная, д. 1","Fact_Address":"г. Москва, ул. Примерная, д. 1"}
		]}`)
	})
	host, port := splitTestServerURL(t, srv.URL)

	c := New(5 * time.Second)
	info, err := c.FetchInfo(context.Background(), host, port)
	if err != nil {
		t.Fatalf("FetchInfo failed: %v", err)
	}

	if info.FSRARID != "030000000001" {
		t.Errorf("FSRARID = %q, want 030000000001", info.FSRARID)
	}
	if info.EgaisCertFrom == nil || info.EgaisCertFrom.UTC().Format("2006-01-02 15:04:05") != "2030-01-01 00:00:00" {
		t.Errorf("EgaisCertFrom = %v", info.EgaisCertFrom)
	}
	if info.EgaisCertTo == nil || info.EgaisCertTo.UTC().Format("2006-01-02 15:04:05") != "2031-01-01 00:00:00" {
		t.Errorf("EgaisCertTo = %v", info.EgaisCertTo)
	}
	if info.GostCertFrom == nil || info.GostCertFrom.UTC().Format("2006-01-02 15:04:05") != "2030-02-01 00:00:00" {
		t.Errorf("GostCertFrom = %v", info.GostCertFrom)
	}
	if info.GostCertTo == nil || info.GostCertTo.UTC().Format("2006-01-02 15:04:05") != "2031-02-01 00:00:00" {
		t.Errorf("GostCertTo = %v", info.GostCertTo)
	}
	if info.InstallAddress != "г. Москва, ул. Примерная, д. 1" {
		t.Errorf("InstallAddress = %q", info.InstallAddress)
	}
	if info.INN != "1234567890" {
		t.Errorf("INN = %q, want 1234567890", info.INN)
	}
	if info.OrgName != "ИП ИВАНОВ ИВАН ИВАНОВИЧ" {
		t.Errorf("OrgName = %q", info.OrgName)
	}
}

// TestFetchInfoDiagnosisFallback covers an older/different УТМ version that
// lacks /api/info/list entirely: FSRAR_ID still comes through via the
// documented /diagnosis endpoint, and the poll counts as successful (ИНН
// and certificate dates stay empty — left for manual entry, per the
// package doc comment).
func TestFetchInfoDiagnosisFallback(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/api/info/list", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/diagnosis", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><CERTIFICATE><CN>030000000002</CN></CERTIFICATE>`)
	})

	host, port := splitTestServerURL(t, srv.URL)

	c := New(5 * time.Second)
	info, err := c.FetchInfo(context.Background(), host, port)
	if err != nil {
		t.Fatalf("FetchInfo failed: %v", err)
	}

	if info.FSRARID != "030000000002" {
		t.Errorf("FSRARID = %q, want 030000000002", info.FSRARID)
	}
	if info.INN != "" {
		t.Errorf("expected empty INN via the /diagnosis-only fallback, got %q", info.INN)
	}
	if info.EgaisCertFrom != nil || info.EgaisCertTo != nil {
		t.Errorf("expected empty cert dates via the /diagnosis-only fallback, got from=%v to=%v", info.EgaisCertFrom, info.EgaisCertTo)
	}
}

func TestParseCertTimeVariants(t *testing.T) {
	cases := map[string]string{
		`"2027-06-19 05:11:45 +0000"`: "2027-06-19", // confirmed real format
		`"2026-12-31"`:                "2026-12-31",
		`"2027-02-14T00:00:00"`:       "2027-02-14",
		`"31.12.2026"`:                "2026-12-31",
		`1798675200`:                  "2026-12-31", // unix seconds
		`1798675200000`:               "2026-12-31", // unix milliseconds
		`null`:                        "",
		`""`:                          "",
	}
	for raw, want := range cases {
		got := parseCertTime(json.RawMessage(raw))
		if want == "" {
			if got != nil {
				t.Errorf("parseCertTime(%s) = %v, want nil", raw, got)
			}
			continue
		}
		if got == nil || got.UTC().Format("2006-01-02") != want {
			t.Errorf("parseCertTime(%s) = %v, want %s", raw, got, want)
		}
	}
}

func splitTestServerURL(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}
