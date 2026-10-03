package govflow

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// 本地资金库操作返回的可判定错误。其它错误（I/O、JSON 损坏等）以原文返回。
var (
	// ErrInvalidAction 表示动作原文不符合 transfer:<账户>:<金额> 的格式。
	ErrInvalidAction = errors.New("malformed action")
	// ErrExecutionRejected 汇总执行资格类失败：时间未到、状态不符、
	// 动作为空或格式错误、余额不足、计算溢出。具体原因见 Error()。
	ErrExecutionRejected = errors.New("execution rejected")
)

type actionError string

func (e actionError) Is(target error) bool { return target == ErrInvalidAction }
func (e actionError) Error() string        { return string(e) }

type executionError struct{ reason string }

func (e *executionError) Is(target error) bool { return target == ErrExecutionRejected }
func (e *executionError) Error() string        { return "execution rejected: " + e.reason }

func execReject(reason string) error { return &executionError{reason: reason} }

// ParseTransfer 解析 transfer:<账户>:<金额>。
// 收款账户不能为空或包含冒号；金额只接受十进制数字表示的正整数，
// 且必须落在有符号 64 位整数范围内。
func ParseTransfer(action string) (account string, amount int64, err error) {
	parts := strings.Split(action, ":")
	if len(parts) != 3 || parts[0] != "transfer" {
		return "", 0, actionError("malformed action: expected transfer:<account>:<amount>, got " + strconv.Quote(action))
	}
	if parts[1] == "" {
		return "", 0, actionError("malformed action: recipient account is empty")
	}
	amount, err = parsePositiveInt64(parts[2])
	if err != nil {
		return "", 0, err
	}
	return parts[1], amount, nil
}

// parsePositiveInt64 只接受纯十进制数字、值为正且不超过 math.MaxInt64。
func parsePositiveInt64(s string) (int64, error) {
	if s == "" {
		return 0, actionError("malformed action: amount must be a positive integer")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, actionError("malformed action: amount must be decimal digits only, got " + strconv.Quote(s))
		}
	}
	value, err := strconv.ParseInt(s, 10, 64)
	if err != nil || value <= 0 {
		// 纯数字串只会因超长越界走到这里（math.MaxInt64 = 9223372036854775807）。
		return 0, actionError("malformed action: amount must be a positive integer within int64, got " + strconv.Quote(s))
	}
	return value, nil
}

// addInt64 返回 a+b，结果超出有符号 64 位整数范围时 ok 为 false。
func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// ---- 唯一的转账规则 ----
//
// 提案首次执行（Execute 的预演）与已保存执行凭据的重放校验
// （validateState 打开状态文件时）必须认可同一笔资金变动：资金库扣除金额、
// 收款账户加款，以及双方各自的前后余额。下面的 transferMovement 与
// transferLedger 是这条规则的唯一实现，两条路径都只调用它，避免同一项业务
// 规则在两处各自维护而逐渐漂移。

// transferMovement 是一笔转账在“资金库扣款 + 收款账户加款”规则下的完整结果。
type transferMovement struct {
	account         string // ParseTransfer 认可的收款账户
	amount          int64  // ParseTransfer 认可的正整数金额
	treasuryBefore  int64
	treasuryAfter   int64
	recipientBefore int64
	recipientAfter  int64
	// parseErr：动作原文不合法时的错误（实现 ErrInvalidAction）。
	// 非 nil 时其余字段无意义，由调用方按各自路径的措辞包装。
	parseErr error
	// recipientOverflow：收款账户起始余额加金额超过 math.MaxInt64。
	// 此时 recipientAfter 不可用。
	recipientOverflow bool
	// insufficient：资金库当前余额不足（严格小于金额）。
	insufficient bool
}

// transferLedger 按动作登记顺序逐笔记账，是执行预演与凭据重放共享的状态：
// 资金库余额随每笔扣款变化，收款账户从未出现过时起始余额为 0，同一账户
// 连续收款时后一笔的起始余额承接前一笔的结果。
type transferLedger struct {
	treasury int64
	balances map[string]int64
}

// newTransferLedger 从一个已提交余额快照构造记账器；balances 可为 nil（空表）。
// 调用方传入的 map 不会被复制，apply 会直接在其上推进，因此两条路径都传入
// 自己的模拟余额表，互不影响已提交状态。
func newTransferLedger(treasury int64, balances map[string]int64) *transferLedger {
	if balances == nil {
		balances = map[string]int64{}
	}
	return &transferLedger{treasury: treasury, balances: balances}
}

// apply 按唯一转账规则把一笔动作原文记到当前账本上，返回这笔转账的完整结果。
// 它一次性算出双方的前后余额与两类失败标志（收款溢出、资金库不足），
// 但不在任何失败时推进账本：资金库与余额表保持动作前状态，调用方据此保证
// 整项执行失败不留任何资金变动。两个失败标志同时给出是为了让两条调用路径
// 各自沿用当前的上报先后——首次执行先报收款溢出、后报资金库不足；凭据重放
// 则先核资金库前后余额（含不足）、再核收款账户前后余额（含溢出）。
//
// 金额恰好等于资金库剩余余额时 treasuryAfter 为 0，属于成功。
func (l *transferLedger) apply(raw string) transferMovement {
	m := transferMovement{treasuryBefore: l.treasury}
	account, amount, err := ParseTransfer(raw)
	if err != nil {
		m.parseErr = err
		return m
	}
	m.account = account
	m.amount = amount
	before := l.balances[account]
	m.recipientBefore = before
	after, ok := addInt64(before, amount)
	m.recipientAfter = after
	m.recipientOverflow = !ok
	// amount 为正、l.treasury 非负，l.treasury-amount 落在 int64 范围内，
	// 不足时只是一个负值，仅用于比较，不会被提交。
	m.treasuryAfter = l.treasury - amount
	m.insufficient = l.treasury < amount
	if !m.recipientOverflow && !m.insufficient {
		l.treasury = m.treasuryAfter
		l.balances[account] = m.recipientAfter
	}
	return m
}

// Treasury 返回记账器当前的资金库余额。
func (l *transferLedger) Treasury() int64 { return l.treasury }

// Balances 返回记账器内部的余额表（同一 map，供成功路径整体提交）。
func (l *transferLedger) Balances() map[string]int64 { return l.balances }
