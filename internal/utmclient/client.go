// Package utmclient talks to a УТМ's local HTTP API to discover the
// organization it belongs to (ИНН, name, licensed address) and its
// certificate validity dates.
//
// PRIMARY PATH — confirmed working against a real device (not from the
// official PDF): a small JSON API that backs the УТМ's own web home page
// (http://localhost:8080/app/settings#certificates renders exactly this
// data). All three calls are synchronous and local — no round trip to the
// central ЕГАИС server, because a УТМ only ever serves the one
// organization it's licensed for, so its own identity is already known
// locally rather than looked up live.
//
//	GET /api/info/list -> {"version": "...", "ownerId": "...",
//	  "rsa": {"startDate": "...", "expireDate": "..."},
//	  "gost": {"startDate": "...", "expireDate": "..."}}
//	  ownerId is the FSRAR_ID; rsa/gost carry both dates of each
//	  certificate's validity period.
//	GET /api/query/proxy/gateway/fsm/utm/organizations -> [{"owner_ID": "...",
//	  "full_Name": "...", "short_Name": "...", "inn": "...",
//	  "dejure_Address": "...", "fact_Address": "..."}]
//	  a one-entry (per owner_ID) array with the organization's own ИНН,
//	  name and address — no "kpp" field was present in the one confirmed
//	  sample, an individual entrepreneur (ИП), which has no KPP by law;
//	  a legal entity's response may include one.
//	GET /api/rsa -> {"rows": [{"pass_owner_id": "...", "Fact_Address": "..."}, ...]}
//	  filtering rows by pass_owner_id == ownerId gives the same
//	  installation address as a fallback, in case /organizations doesn't.
//
// This is undocumented in ФСРАР's public "Технические требования УТМ"
// PDF (which only covers the document-exchange protocol), so field names
// beyond what's been confirmed above are unverified on other УТМ
// versions/builds — every raw response is kept (Info.RawResponse)
// specifically so the mapping can be extended if needed.
//
// SECONDARY PATH — used only when the primary path above doesn't cover
// something (an older/different УТМ version lacking one of these
// endpoints, or a legal entity's ИНН/name if /organizations ever comes
// back empty for some device):
//
//  1. GET /diagnosis returns the RSA certificate's subject fields as XML;
//     its CN is the FSRAR_ID (used only if /api/info/list didn't already
//     provide one).
//  2. A QueryClients/QueryPartner document, addressed to that FSRAR_ID as
//     both Owner and query subject ("СИО"), is POSTed as multipart field
//     "xml_file" to /opt/in/QueryPartner. This is a genuinely ASYNC round
//     trip through the central ЕГАИС server (documented in the PDF) that
//     can take a couple of minutes, or simply never complete in practice
//     — real-world software (1С) treats this the same way this client
//     does: a "Запросить из ЕГАИС"-style convenience, not something to
//     block on, with manual entry as the standing alternative (see the
//     "Изменить УТМ" page). Its ReplyPartner reply carries ИНН, КПП,
//     FullName/ShortName and address.
//  3. A best-effort regex scrape of the legacy GET /home page for
//     certificate "from" dates, which neither JSON endpoint provides.
//
// None of these secondary lookups can turn an otherwise-successful poll
// into a failure — only a total inability to determine even the FSRAR_ID
// does that. Every field left zero/nil in the returned Info means "this
// poll didn't determine it"; callers keep whatever value they already had.
package utmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is the port a УТМ's local HTTP API listens on out of the box.
const DefaultPort = 8080

const (
	apiInfoListPath        = "/api/info/list"
	apiOrganizationsPath   = "/api/query/proxy/gateway/fsm/utm/organizations"
	apiRSAPath             = "/api/rsa"
	diagnosisPath          = "/diagnosis"
	queryPartnerSubmitPath = "/opt/in/QueryPartner"
	optOutPath             = "/opt/out"
	homePath               = "/home"
)

// replyWaitTimeout bounds how long we wait for УТМ to relay a QueryPartner
// reply from the central ЕГАИС server — a real network round trip to a
// government server, not a local call, so it can genuinely take a while.
// Safe to keep generous because polling always runs detached from any HTTP
// request (see scheduler.PollOneAsync), and because its result is now only
// a best-effort supplement (ИНН/name), never load-bearing for the poll to
// count as successful.
const (
	replyWaitTimeout  = 120 * time.Second
	replyPollInterval = 3 * time.Second
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

	// The slow, async, documented QueryPartner fallback only runs when the
	// fast local /organizations lookup inside fetchAPIInfo didn't already
	// fill in the ИНН — normally the common case, so this whole block is
	// usually skipped entirely.
	if info.INN == "" {
		if partner, partnerRaw, err := c.fetchPartnerInfo(ctx, base, info.FSRARID); err == nil {
			log.Printf("utmclient: %s: получен ReplyPartner, ИНН=%s", base, partner.INN)
			info.INN = partner.INN
			info.KPP = partner.KPP
			info.OrgName = firstNonEmpty(partner.ShortName, partner.FullName)
			if info.InstallAddress == "" {
				info.InstallAddress = partner.Address
			}
			if partnerRaw != "" {
				info.RawResponse += "\n--- QueryPartner/ReplyPartner ---\n" + partnerRaw
			}
		} else {
			log.Printf("utmclient: %s: QueryPartner (ИНН/название организации, необязательно) failed: %v", base, err)
		}
	}

	if info.EgaisCertFrom == nil || info.GostCertFrom == nil {
		if ef, et, gf, gt, ok := c.scrapeCertDates(ctx, base); ok {
			if info.EgaisCertFrom == nil {
				info.EgaisCertFrom = ef
			}
			if info.EgaisCertTo == nil {
				info.EgaisCertTo = et
			}
			if info.GostCertFrom == nil {
				info.GostCertFrom = gf
			}
			if info.GostCertTo == nil {
				info.GostCertTo = gt
			}
		}
	}

	return info, nil
}

// apiInfoListResponse mirrors GET /api/info/list, confirmed against a real
// УТМ 4.2.0 (prod contour). Sample response:
//
//	{"version":"4.2.0","ownerId":"030001122298",
//	 "rsa":{"certType":"RSA","startDate":"2026-06-19 05:01:45 +0000",
//	        "expireDate":"2027-06-19 05:11:45 +0000","isValid":"valid",
//	        "issuer":"pki.fsrar.ru"},
//	 "gost":{"certType":"GOST","startDate":"2026-06-18 20:14:32 +0000",
//	         "expireDate":"2027-09-18 20:14:32 +0000","isValid":"valid",
//	         "issuer":"ООО \"Компания \"Тензор\""}}
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

type apiRSAResponse struct {
	Rows []struct {
		PassOwnerID string `json:"pass_owner_id"`
		FactAddress string `json:"Fact_Address"`
	} `json:"rows"`
}

// organizationEntry mirrors one row of
// GET /api/query/proxy/gateway/fsm/utm/organizations, confirmed against a
// real device via its browser DevTools Network tab. Unlike QueryPartner,
// this answers instantly with no round trip to the central ЕГАИС server —
// a УТМ only ever serves the one organization it's licensed for, so this
// is just its own already-known identity, not a live lookup.
type organizationEntry struct {
	OwnerID       string `json:"owner_ID"`
	FullName      string `json:"full_Name"`
	ShortName     string `json:"short_Name"`
	INN           string `json:"inn"`
	KPP           string `json:"kpp"` // absent for individual entrepreneurs (ИП); unconfirmed for legal entities
	DejureAddress string `json:"dejure_Address"`
	FactAddress   string `json:"fact_Address"`
}

// fetchAPIInfo is the primary discovery path. It sets whatever it can
// directly on info and returns an error only when /api/info/list itself
// couldn't be read/parsed or didn't carry an ownerId — failures of the
// secondary /organizations and /api/rsa calls are logged but not fatal.
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

	if orgBody, err := c.get(ctx, base+apiOrganizationsPath); err != nil {
		log.Printf("utmclient: %s: %s (ИНН/название, необязательно) failed: %v", base, apiOrganizationsPath, err)
	} else {
		info.RawResponse += "\n--- " + apiOrganizationsPath + " ---\n" + string(orgBody)
		var orgs []organizationEntry
		if err := json.Unmarshal(orgBody, &orgs); err != nil {
			log.Printf("utmclient: %s: разбор %s: %v", base, apiOrganizationsPath, err)
		} else if org := findOrganization(orgs, resp.OwnerID); org != nil {
			info.INN = org.INN
			info.KPP = org.KPP
			info.OrgName = firstNonEmpty(org.ShortName, org.FullName)
			info.InstallAddress = firstNonEmpty(org.FactAddress, org.DejureAddress)
			log.Printf("utmclient: %s: %s ok: ИНН=%s", base, apiOrganizationsPath, org.INN)
		}
	}

	if info.InstallAddress == "" {
		if addrBody, err := c.get(ctx, base+apiRSAPath); err != nil {
			log.Printf("utmclient: %s: %s (адрес установки, необязательно) failed: %v", base, apiRSAPath, err)
		} else {
			info.RawResponse += "\n--- " + apiRSAPath + " ---\n" + string(addrBody)
			var rsaResp apiRSAResponse
			if err := json.Unmarshal(addrBody, &rsaResp); err != nil {
				log.Printf("utmclient: %s: разбор %s: %v", base, apiRSAPath, err)
			} else {
				for _, row := range rsaResp.Rows {
					if row.PassOwnerID == resp.OwnerID && row.FactAddress != "" {
						info.InstallAddress = row.FactAddress
						break
					}
				}
			}
		}
	}

	return nil
}

// findOrganization returns the entry matching ownerID, or the sole entry
// if there's exactly one and none matches by ID (defensive: the field
// name/exact matching semantics aren't confirmed beyond the one sample
// seen, a single individual entrepreneur).
func findOrganization(orgs []organizationEntry, ownerID string) *organizationEntry {
	for i := range orgs {
		if orgs[i].OwnerID == ownerID {
			return &orgs[i]
		}
	}
	if len(orgs) == 1 {
		return &orgs[0]
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

	log.Printf("utmclient: %s: QueryPartner отправлен, ждём ReplyPartner (replyId=%s)", base, replyID)
	docURL, err := c.waitForReply(ctx, base, replyID)
	if err != nil {
		// Keep the receipt visible in RawResponse even on timeout: it proves
		// the УТМ accepted the submission and which replyId we were waiting
		// on, which is the key fact when diagnosing "reply never arrived".
		return nil, string(receiptBody), err
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
	attempt := 0
	for {
		attempt++
		body, err := c.get(ctx, fmt.Sprintf("%s%s?replyId=%s", base, optOutPath, replyID))
		if err != nil {
			log.Printf("utmclient: %s: /opt/out?replyId=%s попытка %d: %v", base, replyID, attempt, err)
		} else {
			var listing struct {
				XMLName xml.Name `xml:"A"`
				URLs    []struct {
					Value string `xml:",chardata"`
				} `xml:"url"`
			}
			if xml.Unmarshal(body, &listing) == nil && len(listing.URLs) > 0 {
				if url := strings.TrimSpace(listing.URLs[0].Value); url != "" {
					log.Printf("utmclient: %s: ReplyPartner получен после %d попыт(ки/ок): %s", base, attempt, url)
					return url, nil
				}
			}
			if attempt%5 == 0 {
				elapsed := (replyWaitTimeout - time.Until(deadline)).Round(time.Second)
				log.Printf("utmclient: %s: /opt/out?replyId=%s попытка %d — ещё не пришло (%s с начала ожидания)", base, replyID, attempt, elapsed)
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
// ranges off the legacy home page — neither JSON API endpoint provides a
// "from" date. Not part of any documented/confirmed contract; may not
// match every УТМ version.
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
