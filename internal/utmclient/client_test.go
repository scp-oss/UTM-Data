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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newFakeUTMServer simulates just enough of a real УТМ to exercise the
// full diagnosis -> QueryPartner submit -> /opt/out poll -> fetch-and-delete
// flow described in the ФСРАР technical spec. The reply only becomes
// visible on /opt/out a short delay after submission, mimicking the real
// async round trip to the central ЕГАИС server.
func newFakeUTMServer(t *testing.T) *httptest.Server {
	t.Helper()
	const replyID = "11111111-1111-1111-1111-111111111111"

	var delivered atomic.Bool
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/diagnosis", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><CERTIFICATE><CN>030000199312</CN></CERTIFICATE>`)
	})

	mux.HandleFunc("/opt/in/QueryPartner", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		file, _, err := r.FormFile("xml_file")
		if err != nil {
			t.Fatalf("missing xml_file field: %v", err)
		}
		file.Close()

		go func() {
			time.Sleep(30 * time.Millisecond)
			delivered.Store(true)
		}()
		fmt.Fprintf(w, `<A><url>%s</url><sign>deadbeef</sign><ver>2</ver></A>`, replyID)
	})

	mux.HandleFunc("/opt/out", func(w http.ResponseWriter, r *http.Request) {
		if delivered.Load() && r.URL.Query().Get("replyId") == replyID {
			fmt.Fprintf(w, `<A><url fileId="x" replyId="%s" timestamp="now">%s/opt/out/ReplyPartner/752</url><ver>1</ver></A>`, replyID, srv.URL)
			return
		}
		fmt.Fprint(w, `<A><ver>1</ver></A>`)
	})

	mux.HandleFunc("/opt/out/ReplyPartner/752", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST to fetch-and-delete the reply, got %s", r.Method)
		}
		fmt.Fprint(w, `<ns:Documents xmlns:ns="http://fsrar.ru/WEGAIS/WB_DOC_SINGLE_01">
<ns:Owner><ns:FSRAR_ID>3463047</ns:FSRAR_ID></ns:Owner>
<ns:Document><ns:ReplyClient><rc:Clients xmlns:rc="http://fsrar.ru/WEGAIS/ReplyClient" xmlns:oref="http://fsrar.ru/WEGAIS/ClientRef">
<rc:Client><oref:ClientRegId>030000000033</oref:ClientRegId><oref:INN>5020000004</oref:INN><oref:KPP>550002001</oref:KPP>
<oref:FullName>АКЦИОНЕРНОЕ ОБЩЕСТВО "Пример"</oref:FullName><oref:ShortName>АО "Пример"</oref:ShortName>
<oref:address><oref:Country>643</oref:Country><oref:description>644073, РОССИЯ, Г ТОМСК, УЛ ГЛАВНАЯ, 2</oref:description></oref:address>
<oref:State>Active</oref:State></rc:Client>
</rc:Clients></ns:ReplyClient></ns:Document></ns:Documents>`)
	})

	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>
			<div>Период действия ключа доступа к ЕГАИС: 01.01.2025 - 31.12.2026</div>
			<div>Период действия ГОСТ сертификата: 15.02.2025 - 14.02.2027</div>
		</body></html>`)
	})

	return srv
}

func TestFetchInfoFullFlow(t *testing.T) {
	srv := newFakeUTMServer(t)
	host, port := splitTestServerURL(t, srv.URL)

	c := New(5 * time.Second)
	info, err := c.FetchInfo(context.Background(), host, port)
	if err != nil {
		t.Fatalf("FetchInfo failed: %v", err)
	}

	if info.FSRARID != "030000199312" {
		t.Errorf("FSRARID = %q, want 030000199312", info.FSRARID)
	}
	if info.INN != "5020000004" {
		t.Errorf("INN = %q, want 5020000004", info.INN)
	}
	if info.KPP != "550002001" {
		t.Errorf("KPP = %q, want 550002001", info.KPP)
	}
	if info.OrgName != `АО "Пример"` {
		t.Errorf("OrgName = %q", info.OrgName)
	}
	if !strings.Contains(info.InstallAddress, "ТОМСК") {
		t.Errorf("InstallAddress = %q, want it to contain ТОМСК", info.InstallAddress)
	}
	if info.EgaisCertFrom == nil || info.EgaisCertTo == nil {
		t.Fatal("expected EGAIS cert dates to be scraped from /home")
	}
	if info.EgaisCertFrom.Format("2006-01-02") != "2025-01-01" {
		t.Errorf("EgaisCertFrom = %v", info.EgaisCertFrom)
	}
	if info.GostCertTo == nil || info.GostCertTo.Format("2006-01-02") != "2027-02-14" {
		t.Errorf("GostCertTo = %v", info.GostCertTo)
	}
}

// TestFetchInfoPrimaryAPIPath exercises the confirmed-working fast path
// (GET /api/info/list + GET /api/rsa) that a real УТМ was seen answering
// synchronously, without any of the async QueryPartner machinery. This
// path is expected to be the common case in practice.
func TestFetchInfoPrimaryAPIPath(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// This is the real shape confirmed against a live УТМ 4.2.0 (prod
	// contour) — see the package doc comment on apiInfoListResponse.
	mux.HandleFunc("/api/info/list", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"4.2.0","contour":"prod","rsaError":null,"checkInfo":null,
			"ownerId":"030001122298",
			"db":{"createDate":"2026-09-08 15:33:15.047","ownerId":"030001122298"},
			"rsa":{"certType":"RSA","startDate":"2026-06-19 05:01:45 +0000",
				"expireDate":"2027-06-19 05:11:45 +0000","isValid":"valid",
				"issuer":"pki.fsrar.ru","keyStartDate":null,"keyExpireDate":null,"isKeyValid":null},
			"gost":{"certType":"GOST","startDate":"2026-06-18 20:14:32 +0000",
				"expireDate":"2027-09-18 20:14:32 +0000","isValid":"valid",
				"issuer":"ООО \"Компания \"Тензор\"","keyStartDate":"2026-06-18 20:14:31 +0000",
				"keyExpireDate":"2027-09-18 20:14:31 +0000","isKeyValid":"valid"},
			"license":false}`)
	})
	// Real shape confirmed via browser DevTools against the same device.
	mux.HandleFunc("/api/query/proxy/gateway/fsm/utm/organizations", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"owner_ID":"030001122298","full_Name":"ИП СИДОРКИН АЛЕКСЕЙ ВЛАДИМИРОВИЧ",
			"short_Name":"ИП СИДОРКИН АЛЕКСЕЙ ВЛАДИМИРОВИЧ","inn":"583709518258",
			"country_Code":"643","region_Code":"58",
			"dejure_Address":"обл. Пензенская,г.о. город Пенза,г. Пенза,ул. Бородина,д. 2",
			"fact_Address":"обл. Пензенская,г.о. город Пенза,г. Пенза,ул. Бородина,д. 2",
			"isLicense":"false"}]`)
	})
	// /api/rsa is a fallback address source only consulted when
	// /organizations doesn't provide one — it must NOT be hit here.
	mux.HandleFunc("/api/rsa", func(w http.ResponseWriter, r *http.Request) {
		t.Error("/api/rsa should not be queried when /organizations already gave an address")
	})
	// QueryPartner must never be invoked once /organizations already
	// supplied ИНН — hitting it here would mean we're needlessly paying
	// for the slow async round trip this whole rewrite exists to avoid.
	mux.HandleFunc("/opt/in/QueryPartner", func(w http.ResponseWriter, r *http.Request) {
		t.Error("QueryPartner should not be invoked when /organizations already gave ИНН")
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

	if info.FSRARID != "030001122298" {
		t.Errorf("FSRARID = %q, want 030001122298", info.FSRARID)
	}
	if info.EgaisCertFrom == nil || info.EgaisCertFrom.UTC().Format("2006-01-02 15:04:05") != "2026-06-19 05:01:45" {
		t.Errorf("EgaisCertFrom = %v", info.EgaisCertFrom)
	}
	if info.EgaisCertTo == nil || info.EgaisCertTo.UTC().Format("2006-01-02 15:04:05") != "2027-06-19 05:11:45" {
		t.Errorf("EgaisCertTo = %v", info.EgaisCertTo)
	}
	if info.GostCertFrom == nil || info.GostCertFrom.UTC().Format("2006-01-02 15:04:05") != "2026-06-18 20:14:32" {
		t.Errorf("GostCertFrom = %v", info.GostCertFrom)
	}
	if info.GostCertTo == nil || info.GostCertTo.UTC().Format("2006-01-02 15:04:05") != "2027-09-18 20:14:32" {
		t.Errorf("GostCertTo = %v", info.GostCertTo)
	}
	if info.InstallAddress != "обл. Пензенская,г.о. город Пенза,г. Пенза,ул. Бородина,д. 2" {
		t.Errorf("InstallAddress = %q", info.InstallAddress)
	}
	if info.INN != "583709518258" {
		t.Errorf("INN = %q, want 583709518258", info.INN)
	}
	if info.OrgName != "ИП СИДОРКИН АЛЕКСЕЙ ВЛАДИМИРОВИЧ" {
		t.Errorf("OrgName = %q", info.OrgName)
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
