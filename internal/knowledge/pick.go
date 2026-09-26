package knowledge

import "slices"

// Pick selects the cards shown for a request: those of the category whose
// every condition holds for the user's answers, most important first (ties
// keep the given order). Links limited by `when` are kept only for the users
// they are meant for. Every Base implementation picks through this function,
// so the YAML file and the database show the same selections.
func Pick(cards []Card, req Request) []Entry {
	var picked []Card
	for _, c := range cards {
		if slices.Contains(c.Categories, req.Category) && c.Match.holds(req.Answers) {
			picked = append(picked, c)
		}
	}
	slices.SortStableFunc(picked, func(a, b Card) int { return b.Priority - a.Priority })

	out := make([]Entry, 0, len(picked))
	for _, c := range picked {
		out = append(out, c.entry(func(l CardLink) bool { return l.When.holds(req.Answers) }))
	}
	return out
}

// Preview renders the card as a student would see it, with every link: an
// admin checks all of them before approving.
func (c Card) Preview() Entry {
	return c.entry(func(CardLink) bool { return true })
}

func (c Card) entry(keep func(CardLink) bool) Entry {
	e := Entry{
		ID:        c.ID,
		Title:     c.Title,
		Summary:   c.Summary,
		Steps:     c.Steps,
		Documents: c.Documents,
		Where:     c.Where,
	}
	for _, l := range c.Links {
		if keep(l) {
			e.Links = append(e.Links, Link{Title: l.Title, URL: l.URL})
		}
	}
	return e
}
