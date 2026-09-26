// Package knowledge defines how the bot looks up guides for a user.
//
// The bot only depends on the Base interface, so the storage can be swapped
// (static YAML today, a vector database like Qdrant later) without touching
// the dialogue logic.
package knowledge

import "context"

// Base finds knowledge base entries relevant to a user.
type Base interface {
	// Find returns entries matching the request, most relevant first.
	Find(ctx context.Context, req Request) ([]Entry, error)
}

// Request describes who is asking and about what.
type Request struct {
	// Category is a rubricator category ID, e.g. "benefits".
	Category string
	// Answers maps question IDs to the chosen option IDs.
	Answers map[string][]string
}

// Entry is a ready-to-show guide: what it is, why, and where to go.
type Entry struct {
	ID    string
	Title string
	// Summary explains what the benefit is and why the user needs it.
	Summary   string
	Steps     []string
	Documents []string
	// Where tells where to go or apply.
	Where string
	Links []Link
}

// Link points to an official source.
type Link struct {
	Title string
	URL   string
}
