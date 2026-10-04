package govflow

import "fmt"

// 本文件是本地资金库唯一的转账规则实现。
//
// “从资金库扣一笔金额、给指定收款账户加同一笔金额，并记录双方前后余额”
// 这条业务规则同时服务于两条路径：
//   - 提案首次执行（Store.Execute）：在副本上按登记原顺序逐笔预演，
//     全部成功才构造凭据并一次性提交；
//   - 读取已保存执行凭据（validateState）：从初始资金库出发按成功提交
//     次序逐笔重放，核对凭据记载的动作原文、动作编号与前后余额。
//
// 两条路径共用 computeTransfer 的解析、加减与范围判定，成功执行产生的
// 资金变动因此与读取凭据时认可的资金变动必然一致；规则只需在此维护一份。

// transferFailureKind 标识单笔动作在转账规则下失败的具体类别。
type transferFailureKind int

const (
	// transferMalformed：动作原文不符合 transfer:<账户>:<金额>。
	transferMalformed transferFailureKind = iota
	// transferRecipientOverflow：收款账户加款后的余额超出 int64 上限。
	transferRecipientOverflow
	// transferInsufficient：动作金额超过资金库当前剩余余额。
	transferInsufficient
)

// transferRuleError 是单笔动作违反转账规则时的中性描述，不带动作位置与
// 调用场景。位置标注（“action %d”/“receipt ... action %d”）与错误分类
// （执行被拒 / 状态损坏）由两条调用路径各自按既有文案包装。
type transferRuleError struct {
	kind     transferFailureKind
	parseErr error // 仅 transferMalformed：ParseTransfer 的原始错误（匹配 ErrInvalidAction）
	account  string
	amount   int64
	treasury int64 // 判定时的资金库余额
	balance  int64 // 判定时收款账户的起始余额
}

// transferStep 是一笔动作在统一转账规则下的完整判定与资金变动。
// failure 为 nil 时其余字段即该动作认可的扣款/加款与前后余额；
// failure 非 nil 时只有与定位、判定相关的字段有效。
//
// recipientAddOK 与 treasuryShort 是两个相互独立的判定：前者表示收款
// 账户 before+amount 是否仍在 int64 范围内，后者表示资金库是否不足。
// 两者可同时为假（不足且溢出）：首次执行按“先溢出后不足”只报告溢出，
// 凭据重放则按“先资金库侧后收款侧”的字段核对顺序先报告资金库一侧，
// 因此这里分别保留，不由首个失败原因互相推导。
type transferStep struct {
	account         string
	amount          int64
	treasuryBefore  int64
	treasuryAfter   int64
	recipientBefore int64
	recipientAfter  int64
	recipientAddOK  bool // 收款账户 before+amount 是否仍在有符号 64 位整数范围内
	treasuryShort   bool // 动作金额是否超过资金库当前剩余余额
	failure         *transferRuleError
}

// computeTransfer 对单笔动作应用唯一的转账规则：
// 解析动作原文 → 收款账户起始余额加金额（不得溢出 int64）→
// 资金库余额不得小于动作金额 → 给出双方前后余额。
// 收款账户从未出现时起始余额按 0 计；金额恰好等于资金库余额时扣款到 0，
// 仍然合法。执行失败按“动作非法 → 收款溢出 → 资金库不足”的预演顺序
// 在 failure 中给出首个原因；凭据重放路径需要保留自己既有的字段核对
// 顺序时，直接使用结果中的期望值与 recipientAddOK/treasuryShort 两个
// 独立标志逐字段比较。
func computeTransfer(raw string, treasury int64, balances map[string]int64) transferStep {
	st := transferStep{treasuryBefore: treasury, recipientAddOK: true}
	account, amount, err := ParseTransfer(raw)
	if err != nil {
		st.failure = &transferRuleError{kind: transferMalformed, parseErr: err, treasury: treasury}
		return st
	}
	st.account = account
	st.amount = amount
	// nil map 读取零值，等价于“收款账户从未出现，起始余额为 0”。
	st.recipientBefore = balances[account]
	after, ok := addInt64(st.recipientBefore, amount)
	st.recipientAddOK = ok
	if ok {
		st.recipientAfter = after
	}
	st.treasuryShort = treasury < amount
	// 金额为正，treasury-amount 在资金充足时恒可表示；不足时该值只作为
	// 期望值参与凭据字段比较，不会被任何成功路径采纳。
	st.treasuryAfter = treasury - amount
	switch {
	case !ok:
		st.failure = &transferRuleError{
			kind: transferRecipientOverflow, account: account, amount: amount,
			treasury: treasury, balance: st.recipientBefore,
		}
	case st.treasuryShort:
		st.failure = &transferRuleError{
			kind: transferInsufficient, account: account, amount: amount,
			treasury: treasury, balance: st.recipientBefore,
		}
	}
	return st
}

// rejectAsExecution 把规则失败包装成首次执行路径既有的错误：
// 动作格式错误同时匹配 ErrExecutionRejected 与 ErrInvalidAction；
// 收款溢出与资金库不足只匹配 ErrExecutionRejected。文案保留动作位置
// 与具体原因，与整理前完全一致。
func (e *transferRuleError) rejectAsExecution(index int, raw string) error {
	switch e.kind {
	case transferMalformed:
		// 双 %w：调用方既可按 ErrExecutionRejected 判定执行被拒，
		// 也可按 ErrInvalidAction 区分“动作格式错误”这一具体原因。
		return fmt.Errorf("%w: action %d (%q): %w", ErrExecutionRejected, index, raw, e.parseErr)
	case transferRecipientOverflow:
		return execReject(fmt.Sprintf(
			"recipient balance overflow at action %d: account=%s balance=%d amount=%d",
			index, e.account, e.balance, e.amount))
	default: // transferInsufficient
		return execReject(fmt.Sprintf(
			"insufficient treasury balance at action %d: treasury=%d amount=%d",
			index, e.treasury, e.amount))
	}
}

// cloneBalances 复制收款账户余额表；入参为 nil 时返回非 nil 空表。
func cloneBalances(balances map[string]int64) map[string]int64 {
	clone := make(map[string]int64, len(balances))
	for k, v := range balances {
		clone[k] = v
	}
	return clone
}

// planTransfers 在给定起始资金状态上按动作原顺序预演整项提案的全部转账，
// 这是首次执行的第一阶段。任一动作失败即返回带动作位置的错误，已检查过
// 的动作只作用于副本，不会改动起始状态；调用方据此整项拒绝，不留资金
// 变动、成功凭据或已执行状态。全部成功时返回每笔动作的规则结果，以及
// 预演后的资金库余额与完整收款账户余额表。
func planTransfers(actions []string, treasury int64, balances map[string]int64) ([]transferStep, int64, map[string]int64, error) {
	simTreasury := treasury
	simBalances := cloneBalances(balances)
	steps := make([]transferStep, 0, len(actions))
	for i, raw := range actions {
		st := computeTransfer(raw, simTreasury, simBalances)
		if st.failure != nil {
			return nil, 0, nil, st.failure.rejectAsExecution(i, raw)
		}
		steps = append(steps, st)
		// 同一账户连续收款时，后一笔的起始余额承接前一笔结果。
		simTreasury = st.treasuryAfter
		simBalances[st.account] = st.recipientAfter
	}
	return steps, simTreasury, simBalances, nil
}

// actionReceipt 按统一转账规则的计算结果构造凭据中的逐笔留痕。
// 资金库侧账户名固定为 "treasury"，收款侧为动作解析出的账户；
// 逐笔前后余额直接取自规则结果，凭据记载的变动与实际提交的变动同源。
func (s transferStep) actionReceipt(index int, raw string) ActionReceipt {
	return ActionReceipt{
		Index:  index,
		Action: raw,
		Treasury: newBalanceUpdate(
			"treasury", s.treasuryBefore, s.treasuryAfter),
		Recipient: newBalanceUpdate(
			s.account, s.recipientBefore, s.recipientAfter),
	}
}

// verifyReceiptTransfers 按成功提交次序核对一张已保存凭据声明的全部转账：
// 动作条数、动作编号（0 起连续）与动作原文必须与登记一致；每笔的资金库
// 与收款账户前后余额必须等于统一转账规则从 simTreasury/simBalances
// 重放出的结果。核对通过的各笔转账依次应用；返回错误时出错动作及其后
// 续动作不会应用（出错动作之前已核对的动作留在重放状态里），调用方在
// 任一错误下都整份拒绝该状态文件，中间结果不会被查询或落盘采纳。
//
// 字段核对顺序沿用读取路径的既有约定：资金库前余额 → 资金库后余额
// （资金不足在此暴露）→ 收款账户前余额 → 收款账户后余额（加款溢出在此
// 暴露）；因此同一动作同时资金不足且收款溢出时，读取路径先报告资金库
// 一侧，与首次执行先报告溢出的顺序各自保持不变。
func verifyReceiptTransfers(rcpt *Receipt, ownerActions []string, simTreasury int64, simBalances map[string]int64) (int64, error) {
	id := rcpt.ProposalID
	if len(rcpt.Actions) != len(ownerActions) {
		return simTreasury, fmt.Errorf("receipt %q action count %d does not match proposal %d",
			id, len(rcpt.Actions), len(ownerActions))
	}
	for i, ar := range rcpt.Actions {
		if ar.Index != i {
			return simTreasury, fmt.Errorf("receipt %q action %d has index %d", id, i, ar.Index)
		}
		if ar.Action != ownerActions[i] {
			return simTreasury, fmt.Errorf("receipt %q action %d text does not match registered action", id, i)
		}
		st := computeTransfer(ar.Action, simTreasury, simBalances)
		if st.failure != nil && st.failure.kind == transferMalformed {
			return simTreasury, fmt.Errorf("receipt %q action %d is not executable: %v", id, i, st.failure.parseErr)
		}
		if ar.Treasury.Account != "treasury" || ar.Treasury.Before != st.treasuryBefore {
			return simTreasury, fmt.Errorf("receipt %q action %d treasury before-balance mismatch", id, i)
		}
		if st.treasuryShort || ar.Treasury.After != st.treasuryAfter {
			return simTreasury, fmt.Errorf("receipt %q action %d treasury after-balance mismatch", id, i)
		}
		if ar.Recipient.Account != st.account || ar.Recipient.Before != st.recipientBefore {
			return simTreasury, fmt.Errorf("receipt %q action %d recipient before-balance mismatch", id, i)
		}
		if !st.recipientAddOK || ar.Recipient.After != st.recipientAfter {
			return simTreasury, fmt.Errorf("receipt %q action %d recipient after-balance mismatch", id, i)
		}
		simTreasury = st.treasuryAfter
		simBalances[st.account] = st.recipientAfter
	}
	return simTreasury, nil
}
