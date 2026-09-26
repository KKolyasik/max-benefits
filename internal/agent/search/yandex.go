package search

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// YandexURL is the synchronous web search of Yandex Search API v2:
// https://aistudio.yandex.ru/docs/ru/search-api/api-ref/WebSearch/search
const YandexURL = "https://searchapi.api.cloud.yandex.net/v2/web/search"

// Yandex returns this error code when a query simply has no results.
const yandexNoResults = "15"

// Yandex searches through Yandex Search API. It costs about 0.5 ₽ per query.
type Yandex struct {
	APIKey   string
	FolderID string
	// Region is a Yandex region ID: 2 is Saint Petersburg.
	Region int
	URL    string
	HTTP   *http.Client
}

type yandexRequest struct {
	Query struct {
		SearchType  string `json:"searchType"`
		QueryText   string `json:"queryText"`
		FamilyMode  string `json:"familyMode"`
		FixTypoMode string `json:"fixTypoMode"`
	} `json:"query"`
	GroupSpec struct {
		GroupMode    string `json:"groupMode"`
		GroupsOnPage int    `json:"groupsOnPage"`
		DocsInGroup  int    `json:"docsInGroup"`
	} `json:"groupSpec"`
	MaxPassages    int    `json:"maxPassages"`
	Region         string `json:"region"`
	L10n           string `json:"l10n"`
	FolderID       string `json:"folderId"`
	ResponseFormat string `json:"responseFormat"`
}

// Search returns up to limit results, one per site.
func (y *Yandex) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	var body yandexRequest
	body.Query.SearchType = "SEARCH_TYPE_RU"
	body.Query.QueryText = query
	body.Query.FamilyMode = "FAMILY_MODE_MODERATE"
	body.Query.FixTypoMode = "FIX_TYPO_MODE_ON"
	// Deep grouping gives one document per site.
	body.GroupSpec.GroupMode = "GROUP_MODE_DEEP"
	body.GroupSpec.GroupsOnPage = limit
	body.GroupSpec.DocsInGroup = 1
	body.MaxPassages = 3
	body.Region = strconv.Itoa(y.Region)
	body.L10n = "LOCALIZATION_RU"
	body.FolderID = y.FolderID
	body.ResponseFormat = "FORMAT_XML"
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	endpoint := y.URL
	if endpoint == "" {
		endpoint = YandexURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Api-Key "+y.APIKey)
	resp, err := y.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("yandex search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("yandex search: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: Yandex rejected the key (%d); check YANDEX_API_KEY, the folder and the search-api.webSearch.user role",
			ErrUnavailable, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("yandex search: %d %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var r struct {
		RawData string `json:"rawData"`
	}
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("yandex search: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(r.RawData)
	if err != nil {
		return nil, fmt.Errorf("yandex search: %w", err)
	}
	results, err := parseYandexXML(raw)
	if err != nil {
		return nil, err
	}
	return onePerSite(results, limit), nil
}

type yandexXML struct {
	Error *struct {
		Code string `xml:"code,attr"`
		Text string `xml:",chardata"`
	} `xml:"response>error"`
	Docs []struct {
		URL      string      `xml:"url"`
		Title    innerText   `xml:"title"`
		Passages []innerText `xml:"passages>passage"`
	} `xml:"response>results>grouping>group>doc"`
}

// innerText collects all text of an element: titles and passages wrap the
// query words in <hlword>.
type innerText string

func (t *innerText) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var b strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch tok := tok.(type) {
		case xml.CharData:
			b.Write(tok)
		case xml.EndElement:
			if tok.Name == start.Name {
				*t = innerText(strings.Join(strings.Fields(b.String()), " "))
				return nil
			}
		}
	}
}

func parseYandexXML(raw []byte) ([]Result, error) {
	var doc yandexXML
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("yandex search: parse xml: %w", err)
	}
	if doc.Error != nil {
		if doc.Error.Code == yandexNoResults {
			return nil, nil
		}
		return nil, fmt.Errorf("yandex search: error %s: %s", doc.Error.Code, strings.TrimSpace(doc.Error.Text))
	}
	results := make([]Result, 0, len(doc.Docs))
	for _, d := range doc.Docs {
		r := Result{URL: strings.TrimSpace(d.URL), Title: string(d.Title)}
		for _, p := range d.Passages {
			r.Snippets = append(r.Snippets, string(p))
		}
		results = append(results, r)
	}
	return results, nil
}
