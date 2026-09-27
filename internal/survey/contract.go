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

// FromContract returns the survey a message of the contract carries,
// checked like a survey file.
func FromContract(m contract.Survey) (*Survey, error) {
	var f file
	for _, c := range m.Categories {
		f.Categories = append(f.Categories, &Category{ID: c.ID, Title: c.Title, Intro: c.Intro, Questions: c.Questions})
	}
	for _, q := range m.Questions {
		question := &Question{ID: q.ID, Label: q.Label, Text: q.Text, Multi: q.Multi}
		for _, o := range q.Options {
			question.Options = append(question.Options, &Option{ID: o.ID, Title: o.Title, Exclusive: o.Exclusive})
		}
		f.Questions = append(f.Questions, question)
	}
	return build(f)
}
