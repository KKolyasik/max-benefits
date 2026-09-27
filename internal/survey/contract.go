package survey

import "github.com/KKolyasik/max-benefits/contract"

// Contract returns the survey as a message of the contract: the sections
// and the questions asked in them.
func (s *Survey) Contract() contract.Survey {
	var out contract.Survey
	for _, c := range s.Categories {
		out.Categories = append(out.Categories, contract.Category{ID: c.ID, Title: c.Title, Intro: c.Intro, Questions: c.Questions})
	}
	for _, q := range s.AllQuestions() {
		options := make([]contract.Option, len(q.Options))
		for i, o := range q.Options {
			options[i] = contract.Option{ID: o.ID, Title: o.Title, Exclusive: o.Exclusive}
		}
		out.Questions = append(out.Questions, contract.Question{ID: q.ID, Label: q.Label, Text: q.Text, Multi: q.Multi, Options: options})
	}
	return out
}
