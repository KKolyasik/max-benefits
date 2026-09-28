// Package feedback describes what students write to the team from the bot:
// an idea, an option a question lacks, or what they did not find in a
// category. Admins read it in the bot.
package feedback

import "time"

// Feedback is a message from a student.
type Feedback struct {
	ID     int64
	UserID int64
	// Category and Question tell what the feedback is about: both are set for
	// a question with no fitting option, only Category for a category where
	// nothing was found, and neither for an idea from the menu.
	Category string
	Question string
	// Answers are the student's answers to the questions of the category
	// where nothing was found: they show what the base lacks.
	Answers   map[string][]string
	Text      string
	CreatedAt time.Time
}
