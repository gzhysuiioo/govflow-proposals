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
// 只累加所选一侧；该侧结果超出 int64 上限时整票拒绝，提案保持原样，
// 后续仍能容纳的投票不受影响。
func Vote(proposal *Proposal, weight int64, support bool) error {
	if proposal.State != "voting" {
		return errInvalid("proposal is not open for voting")
	}
	if weight <= 0 {
		return errInvalid("vote weight must be positive")
	}
	if support {
		sum, ok := addInt64(proposal.ForVotes, weight)
		if !ok {
			return errInvalid("vote weight overflows int64")
		}
		proposal.ForVotes = sum
	} else {
		sum, ok := addInt64(proposal.AgainstVotes, weight)
		if !ok {
			return errInvalid("vote weight overflows int64")
		}
		proposal.AgainstVotes = sum
	}
	return nil
}

// Tally returns the state a proposal moves to under quorum and majority rules.
// 两侧各自可表示、合计超出 int64 上限时仍按真实参与权重判断法定人数。
func Tally(proposal *Proposal) string {
	if proposal.State != "voting" {
		return proposal.State
	}
	if !turnoutReachesQuorum(proposal.ForVotes, proposal.AgainstVotes, proposal.Quorum) {
		return "rejected"
	}
	if proposal.ForVotes > proposal.AgainstVotes {
		return "passed"
	}
	return "rejected"
}

// turnoutReachesQuorum 在不溢出 int64 的前提下判断 for+against 是否达到 quorum。
// 调用方保证两侧权重非负、quorum 为正。
func turnoutReachesQuorum(forVotes, againstVotes, quorum int64) bool {
	// forVotes >= quorum 时直接达到；否则 quorum-forVotes 为正且不会溢出，
	// 再与 againstVotes 比较即等价于比较两侧之和。
	return forVotes >= quorum || againstVotes >= quorum-forVotes
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
