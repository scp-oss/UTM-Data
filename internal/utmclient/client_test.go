package utmclient

import (
	"context"
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

	u, err := url.Parse(srv.URL)
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
