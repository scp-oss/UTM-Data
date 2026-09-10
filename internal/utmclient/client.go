// Package utmclient talks to a УТМ's local HTTP API to discover the
// organization it belongs to (ИНН, name, licensed address) and, on a
// best-effort basis, its certificate validity dates.
//
// The organization lookup follows the flow documented in ФСРАР's
// "Технические требования УТМ" (https://fsrar.gov.ru/opendata/dist/documentation.pdf):
//
//  1. GET /diagnosis returns the RSA (ЕГАИС access) certificate's subject
//     fields as XML; its CN is the УТМ's own FSRAR_ID.
//  2. A QueryClients/QueryPartner document, addressed to that same
//     FSRAR_ID as both Owner and query subject (query parameter "СИО"),
//     is POSTed as multipart field "xml_file" to /opt/in/QueryPartner.
//     УТМ answers immediately with a receipt whose <url> is a UUID that
//     becomes the eventual reply's replyId — the real answer is an
//     ASYNC round trip through the central ЕГАИС server, not a local
//     lookup, and can take a while.
//  3. /opt/out?replyId=<uuid> is polled until the ReplyPartner document
//     shows up, then fetched-and-removed with POST (per the spec, GET
//     reads without removing, POST reads and deletes, DELETE removes
//     without reading).
//  4. The ReplyPartner XML carries ИНН, КПП, FullName/ShortName and the
//     licensed address.
//
// Certificate validity dates (the EGAIS RSA cert and the GOST cert) are
// NOT part of this documented API at all — the spec only shows them
// displayed on the УТМ's own web home page, never as a JSON/XML field.
// scrapeCertDates makes a best-effort attempt against the legacy GET /home
// page using the exact Russian labels the spec names, but that page's
// actual markup was never shown in the PDF (only described narratively),
// so it may simply not match a given УТМ version — this is explicitly a
// heuristic, not a documented contract. When it finds nothing, existing
// values are left untouched; the "Изменить УТМ" page lets an operator
// enter/correct these dates by hand from the УТМ's own home page or a
// downloaded certificate, which is the officially supported way to see
// them.
package utmclient

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// DefaultPort is the port a УТМ's local HTTP API listens on out of the box.
const DefaultPort = 8080

const (
	diagnosisPath          = "/diagnosis"
	queryPartnerSubmitPath = "/opt/in/QueryPartner"
	optOutPath             = "/opt/out"
	homePath               = "/home"
)

// replyWaitTimeout bounds how long we wait for УТМ to relay a reply from
// the central ЕГАИС server — a real network round trip to a government
// server, not a local call, so it needs much more room than a typical
// request.
const (
	replyWaitTimeout  = 45 * time.Second
	replyPollInterval = 2 * time.Second
)

type Client struct {
	httpClient *http.Client
}

func New(timeout time.Duration) *Client {
	return &Client{httpClient: &http.Client{Timeout: timeout}}
}

// Info is the normalized, possibly-partial result of polling a УТМ. Any
// field left zero/nil means this poll didn't determine it — callers should
// keep whatever value they already had rather than overwrite it.
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
	RawResponse    string // last raw response seen, kept for troubleshooting
}

// FetchInfo runs the full discovery flow against a single УТМ. It returns a
// non-nil *Info even on error, populated with whatever raw response was
// last seen, so the caller can persist it for debugging.
func (c *Client) FetchInfo(ctx context.Context, ip string, port int) (*Info, error) {
	base := fmt.Sprintf("http://%s:%d", ip, port)

	fsrarID, diagRaw, err := c.fetchFSRARID(ctx, base)
	info := &Info{FSRARID: fsrarID, RawResponse: diagRaw}
	if err != nil {
		return info, fmt.Errorf("получение FSRAR_ID (%s%s): %w", base, diagnosisPath, err)
	}

	partner, partnerRaw, err := c.fetchPartnerInfo(ctx, base, fsrarID)
	if partnerRaw != "" {
		info.RawResponse = partnerRaw
	}
	if err != nil {
		return info, fmt.Errorf("получение справочника организации (QueryPartner): %w", err)
	}
	info.INN = partner.INN
	info.KPP = partner.KPP
	info.OrgName = firstNonEmpty(partner.ShortName, partner.FullName)
	info.InstallAddress = partner.Address

	// Best effort only — see package doc comment above. A miss here does
	// not fail the whole poll; INN/org/address are already good.
	if ef, et, gf, gt, ok := c.scrapeCertDates(ctx, base); ok {
		info.EgaisCertFrom, info.EgaisCertTo = ef, et
		info.GostCertFrom, info.GostCertTo = gf, gt
	}

	return info, nil
}

type diagnosisCert struct {
	XMLName xml.Name `xml:"CERTIFICATE"`
	CN      string   `xml:"CN"`
}

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

type partnerInfo struct {
	INN       string
	KPP       string
	FullName  string
	ShortName string
	Address   string
}

// replyPartnerDoc mirrors the ReplyPartner XML shape from the spec. Struct
// tags deliberately omit namespace prefixes (ns:, rc:, oref:) — encoding/xml
// matches on local name when no namespace is given in the tag, which is
// enough here since the field names aren't reused elsewhere in the document.
type replyPartnerDoc struct {
	XMLName  xml.Name `xml:"Documents"`
	Document struct {
		ReplyClient struct {
			Clients struct {
				Client []struct {
					INN       string `xml:"INN"`
					KPP       string `xml:"KPP"`
					FullName  string `xml:"FullName"`
					ShortName string `xml:"ShortName"`
					Address   struct {
						Description string `xml:"description"`
					} `xml:"address"`
				} `xml:"Client"`
			} `xml:"Clients"`
		} `xml:"ReplyClient"`
	} `xml:"Document"`
}

func (c *Client) fetchPartnerInfo(ctx context.Context, base, fsrarID string) (*partnerInfo, string, error) {
	receiptBody, err := c.postMultipart(ctx, base+queryPartnerSubmitPath, "xml_file", "query.xml", buildQueryPartnerXML(fsrarID))
	if err != nil {
		return nil, string(receiptBody), fmt.Errorf("отправка QueryPartner: %w", err)
	}

	var receipt struct {
		XMLName xml.Name `xml:"A"`
		URL     string   `xml:"url"`
	}
	if err := xml.Unmarshal(receiptBody, &receipt); err != nil || strings.TrimSpace(receipt.URL) == "" {
		return nil, string(receiptBody), fmt.Errorf("не удалось разобрать квитанцию УТМ")
	}
	replyID := strings.TrimSpace(receipt.URL)

	docURL, err := c.waitForReply(ctx, base, replyID)
	if err != nil {
		return nil, "", err
	}

	replyBody, err := c.postAndConsume(ctx, docURL)
	if err != nil {
		return nil, string(replyBody), fmt.Errorf("получение ReplyPartner: %w", err)
	}

	var doc replyPartnerDoc
	if err := xml.Unmarshal(replyBody, &doc); err != nil {
		return nil, string(replyBody), fmt.Errorf("разбор ReplyPartner: %w", err)
	}
	clients := doc.Document.ReplyClient.Clients.Client
	if len(clients) == 0 {
		return nil, string(replyBody), fmt.Errorf("ReplyPartner не содержит данных об организации")
	}
	cl := clients[0]
	return &partnerInfo{
		INN:       cl.INN,
		KPP:       cl.KPP,
		FullName:  cl.FullName,
		ShortName: cl.ShortName,
		Address:   strings.TrimSpace(cl.Address.Description),
	}, string(replyBody), nil
}

// waitForReply polls /opt/out for the specific replyId our QueryPartner
// submission was given, returning the full URL of the resulting document.
func (c *Client) waitForReply(ctx context.Context, base, replyID string) (string, error) {
	deadline := time.Now().Add(replyWaitTimeout)
	for {
		body, err := c.get(ctx, fmt.Sprintf("%s%s?replyId=%s", base, optOutPath, replyID))
		if err == nil {
			var listing struct {
				XMLName xml.Name `xml:"A"`
				URLs    []struct {
					Value string `xml:",chardata"`
				} `xml:"url"`
			}
			if xml.Unmarshal(body, &listing) == nil && len(listing.URLs) > 0 {
				if url := strings.TrimSpace(listing.URLs[0].Value); url != "" {
					return url, nil
				}
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("ответ ЕГАИС (ReplyPartner) не получен за %s — проверьте, что УТМ подключен к серверу ЕГАИС", replyWaitTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(replyPollInterval):
		}
	}
}

const queryPartnerTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<ns:Documents Version="1.0"
xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:ns="http://fsrar.ru/WEGAIS/WB_DOC_SINGLE_01"
xmlns:qp="http://fsrar.ru/WEGAIS/QueryParameters">
<ns:Owner>
  <ns:FSRAR_ID>%[1]s</ns:FSRAR_ID>
</ns:Owner>
<ns:Document>
<ns:QueryClients>
  <qp:Parameters>
    <qp:Parameter>
      <qp:Name>СИО</qp:Name>
      <qp:Value>%[1]s</qp:Value>
    </qp:Parameter>
  </qp:Parameters>
</ns:QueryClients>
</ns:Document>
</ns:Documents>
`

// buildQueryPartnerXML asks for the УТМ's own organization/subdivision
// record by querying "СИО" (== FSRAR_ID) rather than "ИНН" — the latter
// would require already knowing the very INN we're trying to discover.
func buildQueryPartnerXML(fsrarID string) []byte {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(fsrarID))
	return []byte(fmt.Sprintf(queryPartnerTemplate, escaped.String()))
}

// scrapeCertDates makes a best-effort attempt to read certificate validity
// ranges off the legacy home page. See the package doc comment: this is
// not part of the documented API and may not match every УТМ version.
func (c *Client) scrapeCertDates(ctx context.Context, base string) (egaisFrom, egaisTo, gostFrom, gostTo *time.Time, ok bool) {
	body, err := c.get(ctx, base+homePath)
	if err != nil {
		return nil, nil, nil, nil, false
	}
	html := string(body)

	ef, et, eok := extractDateRangeNear(html, "Период действия ключа доступа к ЕГАИС")
	gf, gt, gok := extractDateRangeNear(html, "Период действия ГОСТ сертификата")
	if !eok && !gok {
		return nil, nil, nil, nil, false
	}
	return ef, et, gf, gt, true
}

var dateRe = regexp.MustCompile(`(\d{2}\.\d{2}\.\d{4})`)

func extractDateRangeNear(html, label string) (*time.Time, *time.Time, bool) {
	idx := strings.Index(html, label)
	if idx == -1 {
		return nil, nil, false
	}
	window := html[idx:min(len(html), idx+400)]
	matches := dateRe.FindAllString(window, 2)
	if len(matches) < 2 {
		return nil, nil, false
	}
	from, err1 := time.Parse("02.01.2006", matches[0])
	to, err2 := time.Parse("02.01.2006", matches[1])
	if err1 != nil || err2 != nil {
		return nil, nil, false
	}
	return &from, &to, true
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) postAndConsume(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) postMultipart(ctx context.Context, url, field, filename string, content []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(content); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
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
