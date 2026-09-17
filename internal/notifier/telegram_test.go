package notifier

import (
	"strings"
	"testing"

	"github.com/scp-oss/utm-data/internal/models"
)

func TestBuildRequestDirect(t *testing.T) {
	endpoint, _, err := buildRequest(models.Settings{TelegramMode: models.TelegramModeDirect, TelegramBotToken: "TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://api.telegram.org/botTOKEN/sendMessage"
	if endpoint != want {
		t.Errorf("endpoint = %q, want %q", endpoint, want)
	}
}

func TestBuildRequestRelay(t *testing.T) {
	endpoint, _, err := buildRequest(models.Settings{
		TelegramMode:         models.TelegramModeRelay,
		TelegramBotToken:     "TOKEN",
		TelegramRelayBaseURL: "https://example-relay.test/",
		TelegramRelayAuthKey: "MYKEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://example-relay.test/botTOKEN/sendMessage?auth=MYKEY"
	if endpoint != want {
		t.Errorf("endpoint = %q, want %q", endpoint, want)
	}
}

func TestBuildRequestRelayMissingBase(t *testing.T) {
	_, _, err := buildRequest(models.Settings{TelegramMode: models.TelegramModeRelay, TelegramBotToken: "TOKEN"})
	if err == nil {
		t.Fatal("expected error for missing relay base URL")
	}
}

func TestBuildRequestSocks5(t *testing.T) {
	endpoint, client, err := buildRequest(models.Settings{
		TelegramMode:     models.TelegramModeSocks5,
		TelegramBotToken: "TOKEN",
		TelegramProxyURL: "socks5://user:pass@127.0.0.1:1080",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(endpoint, "https://api.telegram.org/botTOKEN/") {
		t.Errorf("unexpected endpoint for socks5 mode: %q", endpoint)
	}
	if client.Transport == nil {
		t.Error("expected a custom transport dialing through the SOCKS5 proxy")
	}
}

func TestBuildRequestSocks5MissingProxy(t *testing.T) {
	_, _, err := buildRequest(models.Settings{TelegramMode: models.TelegramModeSocks5, TelegramBotToken: "TOKEN"})
	if err == nil {
		t.Fatal("expected error for missing proxy URL")
	}
}
