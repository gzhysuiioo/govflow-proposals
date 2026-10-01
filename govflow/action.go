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
