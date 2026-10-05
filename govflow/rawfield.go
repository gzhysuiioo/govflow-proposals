package govflow

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是状态文件中“必填字段”的统一读取与判定。
//
// 逐票明细（representative/weight/support/voted_at）、首次计票结果
// （for_weight/against_weight）、执行凭据（executed_at/order、逐笔动作的
// index）与投票提案的委托列表（delegations）遵循同一条字段规则：字段必须
// 明确写出、不得为 null、JSON 类型必须与声明一致（整数另要求 int64 范围内的
// 整数字面量，小数/指数/超界同样拒绝；委托列表必须是 JSON 数组）。
// 缺失或 null 绝不补成零值：明确写出的 false 是有效反对票，窗口允许时明确
// 写出的 0 是有效时间、权重或顺序编号，明确写出的空数组是“无人委托”。
//
// 各类记录共用这里的判定，又各自把字段名与原因短语格式化为带业务位置
// （提案编号、票据下标、计票记录标识）的错误，不会退化成没有记录位置的
// 通用错误。

// scalarKind 是必填字段期望的 JSON 类型；其字符串形式与错误信息中
// “want %s”的用词逐字一致。
type scalarKind string

const (
	scalarString  scalarKind = "string"
	scalarBoolean scalarKind = "boolean"
	scalarInteger scalarKind = "integer" // int64 范围内的整数字面量
	scalarArray   scalarKind = "array"   // JSON 数组（元素形状由解码器与业务校验核对）
)

// rawField 把一个必填字段的字段名、期望类型与解码时接住的原始 JSON
// 放在一起，供 validateRawFields 按声明顺序逐字段判定。
type rawField struct {
	name string
	kind scalarKind
	raw  json.RawMessage
}

// validateRawFields 按声明顺序判定各必填字段，返回第一个不合要求字段的错误；
// 字段全部合法时返回 nil。locate 把字段名与原因短语格式化为带业务位置的错误，
// 判定顺序因此完全由 fields 的声明顺序决定，与记录类型无关。
func validateRawFields(fields []rawField, locate func(field, problem string) error) error {
	for _, f := range fields {
		if problem := requiredScalarProblem(f.raw, f.kind); problem != "" {
			return locate(f.name, problem)
		}
	}
	return nil
}

// requiredScalarProblem 统一判定一个必填字段的原始 JSON：
// 字段未写出、显式为 null 或 JSON 类型不符都返回具体原因短语；
// 合法（含明确写出的 false、0 与空数组）返回空串。
func requiredScalarProblem(raw json.RawMessage, kind scalarKind) string {
	switch {
	case raw == nil:
		return "is missing"
	case string(raw) == "null":
		return "is null"
	case !scalarTypeMatches(raw, kind):
		return fmt.Sprintf("has wrong type: want %s, got %s", kind, jsonValueType(raw))
	}
	return ""
}

// scalarTypeMatches 判定原始 JSON 是否确实是期望类型的值。
func scalarTypeMatches(raw json.RawMessage, kind scalarKind) bool {
	switch kind {
	case scalarString:
		return jsonValueType(raw) == "string"
	case scalarBoolean:
		return jsonValueType(raw) == "boolean"
	case scalarInteger:
		return isInt64Number(raw)
	case scalarArray:
		return jsonValueType(raw) == "array"
	}
	return false
}

// fillString 仅在原始 JSON 确实是字符串时把值解进 dst；否则 dst 保持零值，
// 类型不符留待校验拒绝，绝不能让错误类型悄悄落成零值并参与查询或计票。
func fillString(raw json.RawMessage, dst *string) {
	if jsonValueType(raw) == "string" {
		_ = json.Unmarshal(raw, dst)
	}
}

// fillBool 与 fillString 同理，目标类型为布尔值。
func fillBool(raw json.RawMessage, dst *bool) {
	if jsonValueType(raw) == "boolean" {
		_ = json.Unmarshal(raw, dst)
	}
}

// fillInt64 与 fillString 同理，目标类型为 int64 整数。
func fillInt64(raw json.RawMessage, dst *int64) {
	if isInt64Number(raw) {
		_ = json.Unmarshal(raw, dst)
	}
}

// jsonValueType 按字段实际写出 JSON 的首字节归类其 JSON 类型。
// RawMessage 是从合法 JSON 文档中取出的单个值，首字节足以区分
// 字符串/布尔/null/数字/数组/对象，无需再次解码。
func jsonValueType(raw json.RawMessage) string {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '"':
			return "string"
		case 't', 'f':
			return "boolean"
		case 'n':
			return "null"
		case '[':
			return "array"
		case '{':
			return "object"
		default:
			return "number"
		}
	}
	return "invalid json"
}

// isInt64Number 判断原始 JSON 是否为落在有符号 64 位整数范围内的整数字面量：
// 必须是 JSON 数字（首字节为数字或 '-'），且能被 ParseInt 以 10 进制精确解析。
// 小数、指数、超界整数均返回 false。
func isInt64Number(raw json.RawMessage) bool {
	if jsonValueType(raw) != "number" {
		return false
	}
	_, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return err == nil
}
