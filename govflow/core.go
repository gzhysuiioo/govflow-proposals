// Package govflow implements proposal lifecycle and treasury execution.
package govflow

import "sort"

// Proposal is one governance proposal with its voting window.
type Proposal struct {
	ID           string
	Title        string
	State        string
	ForVotes     int64
	AgainstVotes int64
	Quorum       int64
	TimelockEnd  int64
	Actions      []string
}

// Vote records a weighted vote against an open proposal.
func Vote(proposal *Proposal, weight int64, support bool) error {
	if proposal.State != "voting" {
		return errInvalid("proposal is not open for voting")
	}
	if weight <= 0 {
		return errInvalid("vote weight must be positive")
	}
	if support {
		proposal.ForVotes += weight
	} else {
		proposal.AgainstVotes += weight
	}
	return nil
}

// Tally returns the state a proposal moves to under quorum and majority rules.
func Tally(proposal *Proposal) string {
	if proposal.State != "voting" {
		return proposal.State
	}
	turnout := proposal.ForVotes + proposal.AgainstVotes
	if turnout < proposal.Quorum {
		return "rejected"
	}
	if proposal.ForVotes > proposal.AgainstVotes {
		return "passed"
	}
	return "rejected"
}

// Executable lists passed proposals whose timelock elapsed, stable by id.
func Executable(proposals []Proposal, now int64) []string {
	var ready []string
	for _, proposal := range proposals {
		if proposal.State == "passed" && now >= proposal.TimelockEnd && len(proposal.Actions) > 0 {
			ready = append(ready, proposal.ID)
		}
	}
	sort.Strings(ready)
	return ready
}

type errInvalid string

func (e errInvalid) Error() string { return string(e) }
