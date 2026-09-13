// Package utmclient talks to a УТМ's local HTTP API to discover the
// organization it belongs to (ИНН, name, licensed address) and its
// certificate validity dates.
//
// Confirmed working against a real device (not from the official PDF): a
// small JSON API that backs the УТМ's own web home page
// (http://localhost:8080/app/settings#certificates renders exactly this
// data). All calls are synchronous and local — no round trip to the
// central ЕГАИС server, because a УТМ only ever serves the one
// organization it's licensed for, so its own identity is already known
// locally rather than looked up live. This is why the client no longer
// uses the officially documented but slow/async QueryPartner document
// exchange (submit a query, wait up to minutes for a reply through the
// central server) — it proved unreliable in practice, and everything it
// would provide is available synchronously below instead.
//
//	GET /api/info/list -> {"version": "...", "ownerId": "...",
//	  "rsa": {"startDate": "...", "expireDate": "..."},
//	  "gost": {"startDate": "...", "expireDate": "..."}}
//	  ownerId is the FSRAR_ID; rsa/gost carry both dates of each
//	  certificate's validity period.
//	GET /api/rsa -> {"rows": [{"pass_owner_id": "...", "Owner_ID": "...",
//	  "Full_Name": "...", "Short_Name": "...", "INN": "...", "KPP": "...",
//	  "Dejure_Address": "...", "Fact_Address": "..."}, ...]}
//	  one row per licensed address; every row for a given owner repeats
//	  the same organization fields (ИНН/КПП/name), only the address
//	  differs. Filtering rows by pass_owner_id == ownerId gives the
//	  organization AND its installation address from a single row — only
//	  that matched row is kept, not the full array, to avoid storing the
//	  same organization data dozens of times over in RawResponse.
//
// An earlier revision also queried
// /api/query/proxy/gateway/fsm/utm/organizations (found via browser
// DevTools) for the same organization fields. It turned out to fail
// silently on some devices/polls (proxied through a live backend rather
// than answered locally, unlike the two endpoints above) while /api/rsa
// already carries everything it did, so it was dropped entirely.
//
// This is undocumented in ФСРАР's public "Технические требования УТМ"
// PDF (which only covers the document-exchange protocol), so field names
// beyond what's been confirmed above are unverified on other УТМ
// versions/builds — the matched row is kept (Info.RawResponse)
// specifically so the mapping can be extended if needed.
//
// One lightweight, documented fallback remains for when /api/info/list
// itself is unavailable (an older/different УТМ version): GET /diagnosis
// returns the RSA certificate's subject fields as XML, whose CN is the
// FSRAR_ID. A single local call, not the async document-exchange flow —
// and it cannot turn an otherwise-successful poll into a failure; only a
// total inability to determine even the FSRAR_ID does that.
//
// A legal entity's ИНН/name that /api/rsa doesn't provide, or either
// certificate's "from" date should /api/info/list ever lack one on some
// build, are left for manual entry on the "Изменить УТМ" page (see
// internal/store), the same way 1С treats ИНН/name it couldn't look up
// automatically. Every field left zero/nil in the returned Info means
// "this poll didn't determine it"; callers keep whatever value they
// already had.
package utmclient

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is the port a УТМ's local HTTP API listens on out of the box.
const DefaultPort = 8080

const (
	apiInfoListPath = "/api/info/list"
	apiRSAPath      = "/api/rsa"
	diagnosisPath   = "/diagnosis"
)

type Client struct {
	httpClient *http.Client
}

func New(timeout time.Duration) *Client {
	return &Client{httpClient: &http.Client{Timeout: timeout}}
}

// Info is the normalized, possibly-partial result of polling a УТМ.
type Info struct {
	FSRARID        string
	INN            string
	KPP            string
	OrgName        string
	InstallAddress string
	EgaisCertFrom  *time.Time
	EgaisCertTo    *time.Time
	GostCertFrom   *time.Time
	GostCertTo     *time.Time
	RawResponse    string // raw responses seen so far, kept for troubleshooting
}

// FetchInfo runs the full discovery flow against a single УТМ. It returns a
// non-nil *Info even on error, populated with whatever was determined and
// whatever raw response was last seen, so the caller can persist both for
// debugging. An error is returned only when nothing at all could be
// determined (not even the FSRAR_ID) — a partial result is not an error.
func (c *Client) FetchInfo(ctx context.Context, ip string, port int) (*Info, error) {
	base := fmt.Sprintf("http://%s:%d", ip, port)
	info := &Info{}

	apiErr := c.fetchAPIInfo(ctx, base, info)
	if apiErr != nil {
		log.Printf("utmclient: %s: /api/info/list недоступен (%v), пробуем /diagnosis", base, apiErr)
		if fsrarID, diagRaw, err := c.fetchFSRARID(ctx, base); err == nil {
			info.FSRARID = fsrarID
			if info.RawResponse == "" {
				info.RawResponse = diagRaw
			}
			log.Printf("utmclient: %s: FSRAR_ID (через /diagnosis) = %s", base, fsrarID)
		}
	}

	if info.FSRARID == "" {
		if apiErr != nil {
			return info, fmt.Errorf("получение данных УТМ: %w", apiErr)
		}
		return info, fmt.Errorf("не удалось определить FSRAR_ID")
	}

	if info.INN == "" {
		log.Printf("utmclient: %s: %s не дал ИНН — заполните организацию вручную на странице «Изменить УТМ»", base, apiRSAPath)
	}

	return info, nil
}

// apiInfoListResponse mirrors GET /api/info/list, confirmed against a real
// УТМ 4.2.0 (prod contour). Sample response:
//
//	{"version":"4.2.0","ownerId":"<FSRAR_ID>",
//	 "rsa":{"certType":"RSA","startDate":"2026-01-01 00:00:00 +0000",
//	        "expireDate":"2027-01-01 00:00:00 +0000","isValid":"valid",
//	        "issuer":"pki.fsrar.ru"},
//	 "gost":{"certType":"GOST","startDate":"2026-01-01 00:00:00 +0000",
//	         "expireDate":"2027-01-01 00:00:00 +0000","isValid":"valid",
//	         "issuer":"<удостоверяющий центр>"}}
//
// Dates are still parsed via parseCertTime's json.RawMessage handling
// (rather than a plain string field) since other date encodings were seen
// as speculative before this confirmation and a different УТМ
// version/build could still vary.
type apiInfoListResponse struct {
	Version string `json:"version"`
	OwnerID string `json:"ownerId"`
	RSA     struct {
		StartDate  json.RawMessage `json:"startDate"`
		ExpireDate json.RawMessage `json:"expireDate"`
	} `json:"rsa"`
	Gost struct {
		StartDate  json.RawMessage `json:"startDate"`
		ExpireDate json.RawMessage `json:"expireDate"`
	} `json:"gost"`
}

// apiRSARow is one row of GET /api/rsa, confirmed against a real device.
// Every row for the same owner repeats the same organization fields
// (ИНН/КПП/name) and differs only in address — a УТМ can be licensed for
// several addresses — so only the row matching the polled ownerId is kept.
type apiRSARow struct {
	PassOwnerID   string `json:"pass_owner_id"`
	OwnerID       string `json:"Owner_ID"`
	FullName      string `json:"Full_Name"`
	ShortName     string `json:"Short_Name"`
	INN           string `json:"INN"`
	KPP           string `json:"KPP"` // empty for individual entrepreneurs (ИП), which have no KPP by law
	DejureAddress string `json:"Dejure_Address"`
	FactAddress   string `json:"Fact_Address"`
}

type apiRSAResponse struct {
	Rows []apiRSARow `json:"rows"`
}

// fetchAPIInfo is the primary discovery path. It sets whatever it can
// directly on info and returns an error only when /api/info/list itself
// couldn't be read/parsed or didn't carry an ownerId — a failure of the
// secondary /api/rsa call is logged but not fatal.
func (c *Client) fetchAPIInfo(ctx context.Context, base string, info *Info) error {
	body, err := c.get(ctx, base+apiInfoListPath)
	if len(body) > 0 {
		info.RawResponse = string(body)
	}
	if err != nil {
		return err
	}

	var resp apiInfoListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("разбор %s: %w", apiInfoListPath, err)
	}
	if resp.OwnerID == "" {
		return fmt.Errorf("в ответе %s отсутствует ownerId", apiInfoListPath)
	}

	info.FSRARID = resp.OwnerID
	info.EgaisCertFrom = parseCertTime(resp.RSA.StartDate)
	info.EgaisCertTo = parseCertTime(resp.RSA.ExpireDate)
	info.GostCertFrom = parseCertTime(resp.Gost.StartDate)
	info.GostCertTo = parseCertTime(resp.Gost.ExpireDate)
	log.Printf("utmclient: %s: %s ok: ownerId=%s версия=%s", base, apiInfoListPath, resp.OwnerID, resp.Version)

	if rsaBody, err := c.get(ctx, base+apiRSAPath); err != nil {
		log.Printf("utmclient: %s: %s (организация/адрес, необязательно) failed: %v", base, apiRSAPath, err)
	} else {
		var rsaResp apiRSAResponse
		if err := json.Unmarshal(rsaBody, &rsaResp); err != nil {
			log.Printf("utmclient: %s: разбор %s: %v", base, apiRSAPath, err)
		} else if row := findRSARow(rsaResp.Rows, resp.OwnerID); row != nil {
			info.INN = row.INN
			info.KPP = row.KPP
			info.OrgName = firstNonEmpty(row.ShortName, row.FullName)
			info.InstallAddress = firstNonEmpty(row.FactAddress, row.DejureAddress)
			// Only the matched row is kept, not the full (often 40+ row)
			// array — every row repeats the same organization fields, so
			// storing them all would bloat last_raw_response for nothing.
			if rowJSON, err := json.Marshal(row); err == nil {
				info.RawResponse += "\n--- " + apiRSAPath + " (совпавшая строка) ---\n" + string(rowJSON)
			}
			log.Printf("utmclient: %s: %s ok: ИНН=%s", base, apiRSAPath, row.INN)
		} else {
			log.Printf("utmclient: %s: %s не содержит строки с pass_owner_id=%s", base, apiRSAPath, resp.OwnerID)
		}
	}

	return nil
}

// findRSARow returns the row matching ownerID (by pass_owner_id, falling
// back to Owner_ID), or the sole row if there's exactly one and none
// matches by ID (defensive, mirroring the confirmed real sample).
func findRSARow(rows []apiRSARow, ownerID string) *apiRSARow {
	for i := range rows {
		if rows[i].PassOwnerID == ownerID || rows[i].OwnerID == ownerID {
			return &rows[i]
		}
	}
	if len(rows) == 1 {
		return &rows[0]
	}
	return nil
}

// parseCertTime accepts a quoted date string (the confirmed real layout,
// "2006-01-02 15:04:05 -0700", is tried first; a few other common ones are
// kept as fallbacks in case a different УТМ build/version varies) or a
// bare/quoted Unix timestamp in seconds, milliseconds or microseconds.
func parseCertTime(raw json.RawMessage) *time.Time {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return nil
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		var t time.Time
		switch {
		case n > 1e14:
			t = time.UnixMicro(n)
		case n > 1e11:
			t = time.UnixMilli(n)
		default:
			t = time.Unix(n, 0)
		}
		return &t
	}

	for _, layout := range certTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	log.Printf("utmclient: не удалось разобрать дату сертификата %q", s)
	return nil
}

var certTimeLayouts = []string{
	"2006-01-02 15:04:05 -0700", // confirmed real format, e.g. "2026-06-19 05:01:45 +0000"
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02",
	"02.01.2006",
}

type diagnosisCert struct {
	XMLName xml.Name `xml:"CERTIFICATE"`
	CN      string   `xml:"CN"`
}

// fetchFSRARID is the fallback for obtaining the FSRAR_ID when
// /api/info/list isn't available (older/different УТМ versions) — this
// endpoint IS documented in the official PDF.
func (c *Client) fetchFSRARID(ctx context.Context, base string) (string, string, error) {
	body, err := c.get(ctx, base+diagnosisPath)
	if err != nil {
		return "", string(body), err
	}
	var cert diagnosisCert
	if err := xml.Unmarshal(body, &cert); err != nil {
		return "", string(body), fmt.Errorf("разбор ответа: %w", err)
	}
	if cert.CN == "" {
		return "", string(body), fmt.Errorf("в ответе отсутствует CN (FSRAR_ID)")
	}
	return cert.CN, string(body), nil
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("УТМ вернул статус %d", resp.StatusCode)
	}
	return body, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
