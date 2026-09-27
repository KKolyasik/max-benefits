package knowledge

import "github.com/KKolyasik/max-benefits/contract"

// Contract returns the card as a message of the contract.
func (c Card) Contract() contract.Card {
	links := make([]contract.CardLink, len(c.Links))
	for i, l := range c.Links {
		links[i] = contract.CardLink{Title: l.Title, URL: l.URL, When: l.When}
	}
	return contract.Card{
		ID: c.ID, Categories: c.Categories, Priority: c.Priority, Match: c.Match, Title: c.Title,
		Summary: c.Summary, Steps: c.Steps, Documents: c.Documents, Where: c.Where, Links: links,
	}
}

// CardFromContract returns the card a message of the contract carries.
// Empty lists become nil, as in cards read from YAML.
func CardFromContract(m contract.Card) Card {
	var links []CardLink
	for _, l := range m.Links {
		links = append(links, CardLink{Title: l.Title, URL: l.URL, When: condition(l.When)})
	}
	return Card{
		ID: m.ID, Categories: orNil(m.Categories), Priority: m.Priority, Match: condition(m.Match), Title: m.Title,
		Summary: m.Summary, Steps: orNil(m.Steps), Documents: orNil(m.Documents), Where: m.Where, Links: links,
	}
}

func condition(m map[string][]string) Condition {
	if len(m) == 0 {
		return nil
	}
	return m
}

func orNil(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	return items
}
