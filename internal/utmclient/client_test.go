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
// (GET /api/info/list + GET /api/query/proxy/gateway/fsm/utm/organizations)
// that a real УТМ was seen answering synchronously and locally. This is
// expected to be the common case in practice; fixture values below are
// fictitious (structurally matching the confirmed real shape, but not real
// organization data).
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
	mux.HandleFunc("/api/query/proxy/gateway/fsm/utm/organizations", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"owner_ID":"030000000001","full_Name":"ИП ИВАНОВ ИВАН ИВАНОВИЧ",
			"short_Name":"ИП ИВАНОВ ИВАН ИВАНОВИЧ","inn":"1234567890",
			"country_Code":"643","region_Code":"77",
			"dejure_Address":"г. Москва, ул. Примерная, д. 1",
			"fact_Address":"г. Москва, ул. Примерная, д. 1",
			"isLicense":"false"}]`)
	})
	// /api/rsa is a fallback address source only consulted when
	// /organizations doesn't provide one — it must NOT be hit here.
	mux.HandleFunc("/api/rsa", func(w http.ResponseWriter, r *http.Request) {
		t.Error("/api/rsa should not be queried when /organizations already gave an address")
	})
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
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
// stays empty — left for manual entry, per the package doc comment).
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
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>
			<div>Период действия ключа доступа к ЕГАИС: 01.01.2030 - 31.12.2030</div>
			<div>Период действия ГОСТ сертификата: 01.02.2030 - 28.02.2031</div>
		</body></html>`)
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
	if info.EgaisCertFrom == nil || info.EgaisCertFrom.Format("2006-01-02") != "2030-01-01" {
		t.Errorf("EgaisCertFrom = %v, want it scraped from /home", info.EgaisCertFrom)
	}
	if info.GostCertTo == nil || info.GostCertTo.Format("2006-01-02") != "2031-02-28" {
		t.Errorf("GostCertTo = %v, want it scraped from /home", info.GostCertTo)
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

func TestExtractDateRangeNear(t *testing.T) {
	html := "prefix Период действия ГОСТ сертификата: 15.02.2025 - 14.02.2027 suffix"
	from, to, ok := extractDateRangeNear(html, "Период действия ГОСТ сертификата")
	if !ok {
		t.Fatal("expected a match")
	}
	if from.Format("2006-01-02") != "2025-02-15" || to.Format("2006-01-02") != "2027-02-14" {
		t.Errorf("got from=%v to=%v", from, to)
	}

	if _, _, ok := extractDateRangeNear(html, "not present"); ok {
		t.Error("expected no match for absent label")
	}
}
