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
// 只增加所选一侧的累计权重；累加结果在 [0, math.MaxInt64] 内才接受，
// 恰好达到上限也算成功。超过上限返回说明权重溢出的错误，且不改写任何一侧：
// 不截断、不变负、不挪到另一侧，后续仍能容纳的投票不受影响。
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
			return errInvalid("vote weight overflow: for-side total would exceed 9223372036854775807")
		}
		proposal.ForVotes = sum
	} else {
		sum, ok := addInt64(proposal.AgainstVotes, weight)
		if !ok {
			return errInvalid("vote weight overflow: against-side total would exceed 9223372036854775807")
		}
		proposal.AgainstVotes = sum
	}
	return nil
}

// Tally returns the state a proposal moves to under quorum and majority rules.
// 只返回结论，不改写提案状态、票数或任何执行动作。
func Tally(proposal *Proposal) string {
	if proposal.State != "voting" {
		return proposal.State
	}
	// 两侧各自可表示时合计仍可能超过 int64，直接相加会回绕成负数，
	// 把已达到法定人数的提案误判为参与不足。比较剩余容量即可在不做
	// 上溢加法的前提下判断真实参与量 for+against 是否达到法定人数。
	if proposal.ForVotes < proposal.Quorum-proposal.AgainstVotes {
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
