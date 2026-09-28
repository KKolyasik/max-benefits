package bot

import "strings"

// Callback payloads are "action[:arg[:arg]]". IDs from the survey never
// contain ':' (the survey loader checks it).
const (
	actMenu          = "menu"
	actProfile       = "profile"
	actAbout         = "about"
	actDelete        = "del"
	actDeleteConfirm = "del_yes"
	actCategory      = "cat"  // cat:<category>
	actUseSaved      = "use"  // use:<category>
	actRedo          = "redo" // redo:<category>
	actAnswer        = "ans"  // ans:<question>:<option>, single choice
	actToggle        = "tgl"  // tgl:<question>:<option>, multi choice
	actDone          = "done" // done:<question>, confirms a multi choice
	actBack          = "back" // back:<question>, back to the question before
	actResults       = "res"  // res:<category>, the list of the cards found
	// card:<category>:<part>:<card> is a card of the results. The card ID
	// goes last: cards come from the agent too, and nothing keeps ':' out
	// of their IDs.
	actCard = "card"

	// Admin actions; the bot checks the rights on every press.
	actDrafts  = "adm"      // shows the oldest pending draft
	actApprove = "adm_ok"   // adm_ok:<draft>
	actReject  = "adm_no"   // adm_no:<draft>
	actSkip    = "adm_skip" // adm_skip:<draft>, shows the next one
	actAgent   = "adm_agent"
	actRun     = "adm_run" // adm_run[:force] runs the agent
)

func payload(action string, args ...string) string {
	return strings.Join(append([]string{action}, args...), ":")
}

func parsePayload(s string) (action string, args []string) {
	parts := strings.Split(s, ":")
	return parts[0], parts[1:]
}
