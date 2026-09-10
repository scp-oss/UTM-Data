// Package utmclient polls a УТМ (Универсальный транспортный модуль) instance's
// local HTTP API for organization and certificate information.
//
// ⚠️ ASSUMED SCHEMA — VERIFY AGAINST THE OFFICIAL DOCUMENTATION
//
// This session could not reach https://fsrar.gov.ru (blocked by the sandbox's
// network egress policy), so the endpoint path and XML field names below are
// reconstructed from the publicly documented conventions of ФСРАР's УТМ local
// API (GET /info returning partner/organization data with certificate
// validity ranges), not copied verbatim from the official PDF.
//
// If the real spec differs, THIS FILE is the only place that needs to
// change: adjust InfoPath and the infoResponse struct tags below to match,
// and re-check certificateKind()'s keyword matching against the real
// certificate "type"/"kind" values. Every raw response is stored alongside
// the parsed row (see models.UTM.LastRawResponse) specifically so a real
// payload can be inspected in the web UI and the mapping corrected quickly.
package utmclient

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultPort is the port a УТМ's local HTTP API listens on out of the box.
const DefaultPort = 8080

// InfoPath is the endpoint assumed to return organization + certificate info.
const InfoPath = "/info"

type Client struct {
	httpClient *http.Client
}

func New(timeout time.Duration) *Client {
	return &Client{httpClient: &http.Client{Timeout: timeout}}
}

// Info is the normalized result of polling a УТМ.
type Info struct {
	INN            string
	KPP            string
	OrgName        string
	InstallAddress string
	EgaisCertFrom  *time.Time
	EgaisCertTo    *time.Time
	GostCertFrom   *time.Time
	GostCertTo     *time.Time
	RawResponse    string
}

// infoResponse mirrors the assumed XML schema of GET /info. See the package
// doc comment above for how to adjust it once the real schema is known.
type infoResponse struct {
	XMLName   xml.Name `xml:"PartnerInfo"`
	INN       string   `xml:"INN"`
	KPP       string   `xml:"KPP"`
	ShortName string   `xml:"ShortName"`
	FullName  string   `xml:"FullName"`
	Addresses struct {
		Address []struct {
			Address string `xml:"Address"`
		} `xml:"Address"`
	} `xml:"Addresses"`
	Certificates struct {
		Certificate []struct {
			Type      string `xml:"Type,attr"`
			ValidFrom string `xml:"ValidFrom"`
			ValidTo   string `xml:"ValidTo"`
		} `xml:"Certificate"`
	} `xml:"Certificates"`
}

// FetchInfo polls the given УТМ over HTTP (never HTTPS: the local API is
// plain HTTP by convention) and returns whatever it could parse. The raw
// response body is always returned, even on a parse error, so callers can
// persist it for troubleshooting.
func (c *Client) FetchInfo(ctx context.Context, ip string, port int) (*Info, error) {
	url := fmt.Sprintf("http://%s:%d%s", ip, port, InfoPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	raw := string(body)

	if resp.StatusCode != http.StatusOK {
		return &Info{RawResponse: raw}, fmt.Errorf("УТМ %s вернул статус %d", url, resp.StatusCode)
	}

	var parsed infoResponse
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return &Info{RawResponse: raw}, fmt.Errorf("не удалось разобрать ответ УТМ: %w", err)
	}

	info := toInfo(parsed)
	info.RawResponse = raw
	return info, nil
}

func toInfo(p infoResponse) *Info {
	info := &Info{
		INN:     p.INN,
		KPP:     p.KPP,
		OrgName: firstNonEmpty(p.ShortName, p.FullName),
	}

	addrs := make([]string, 0, len(p.Addresses.Address))
	for _, a := range p.Addresses.Address {
		if a.Address != "" {
			addrs = append(addrs, a.Address)
		}
	}
	info.InstallAddress = strings.Join(addrs, "; ")

	for _, cert := range p.Certificates.Certificate {
		from := parseCertTime(cert.ValidFrom)
		to := parseCertTime(cert.ValidTo)
		switch certificateKind(cert.Type) {
		case certKindEgais:
			info.EgaisCertFrom, info.EgaisCertTo = from, to
		case certKindGost:
			info.GostCertFrom, info.GostCertTo = from, to
		}
	}

	return info
}

type certKind int

const (
	certKindUnknown certKind = iota
	certKindEgais
	certKindGost
)

// certificateKind classifies a certificate "type" attribute leniently,
// since the exact vocabulary used by the real API is unverified.
func certificateKind(t string) certKind {
	lt := strings.ToLower(t)
	switch {
	case strings.Contains(lt, "gost"), strings.Contains(lt, "гост"):
		return certKindGost
	case strings.Contains(lt, "rsa"), strings.Contains(lt, "egais"), strings.Contains(lt, "егаис"), strings.Contains(lt, "transport"):
		return certKindEgais
	default:
		return certKindUnknown
	}
}

var certTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02",
	"02.01.2006 15:04:05",
	"02.01.2006",
}

func parseCertTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range certTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
