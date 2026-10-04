package govflow

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ---- 已保存治理记录的必填标量字段：统一的读取与严格校验 ----
//
// 逐票明细（storedBallot）的 representative/weight/support/voted_at、首次计票
// 结果（storedTally）的 for_weight/against_weight 以及执行凭据（Receipt）的
// executed_at 都是必填标量字段：必须明确写出、非 null 且 JSON 类型正确。
// 绝不能靠零值/默认值补出治理记录——例如缺失或 null 的 support 不得被当作
// false（反对票），缺失或 null 的 voted_at 不得被当作 0（首次投票时间），
// 缺失或 null 的计票权重不得被当作 0（恰好是某侧没有票时的真实零票重）。
//
// 标量字段先用 RawMessage 接住，使“字段缺失”（nil）与“显式 null/写错类型”
// 在解码后仍可区分：encoding/json 直接解进 bool/int64/string 时会把这三种
// 情况都折叠成零值。三类已保存记录共用这一份“缺失/空值/类型不符”的判定，
// 但各自保留自己的业务定位与报错（提案编号 + 票据下标 / 提案编号 + 计票
// 记录标识 / 凭据下标 + 提案编号），不会在整理后变成没有记录位置的通用错误。

// strictFieldKind 是必填标量字段要求的 JSON 类型。
type strictFieldKind int

const (
	strictString  strictFieldKind = iota // 必须是 JSON 字符串
	strictBoolean                        // 必须是 JSON 布尔值（不接受字符串/数字/null）
	strictInteger                        // 必须是 int64 范围内的 JSON 整数（不接受字符串/布尔/小数/指数/null）
)

// strictField 接住一个必填标量字段是否出现及其原始写法（缺失为 nil、
// null 为 "null"），并在字段确实是目标 JSON 类型时填充分类型后的值。
// 单字段类型不符时不在解码阶段报错（value 保持 nil），以免解码器在不含
// 记录定位（提案编号/票据下标/字段名）的通用错误处提前失败；定位与判定
// 统一交给持有该字段的记录的校验函数。
//
// 同一套字段规则同时用于“打开状态文件重放校验”与“本进程新建记录”：
// 投票成功、首次计票与首次执行生成的记录在保存前与重放时必须通过同一份
// 判定，因此构造内存记录时也同步填上原始片段（见 presentStrictField），
// 不会被误判为字段缺失或类型不符。
type strictField struct {
	kind  strictFieldKind
	value any // 类型合规时为 string/bool/int64；缺失、null 或类型不符时为 nil
	raw   json.RawMessage
}

// captureStrictField 按保存格式的字段名逐字接住字段的原始 JSON，并在其确实
// 是目标类型时填充分类型后的值；错误类型只留 raw，绝不能让错误类型悄悄落成
// 零值并参与查询、再次计票或执行。
func captureStrictField(kind strictFieldKind, raw json.RawMessage) strictField {
	f := strictField{kind: kind, raw: raw}
	switch kind {
	case strictString:
		if jsonValueType(raw) == "string" {
			var v string
			if json.Unmarshal(raw, &v) == nil {
				f.value = v
			}
		}
	case strictBoolean:
		if jsonValueType(raw) == "boolean" {
			var v bool
			if json.Unmarshal(raw, &v) == nil {
				f.value = v
			}
		}
	case strictInteger:
		var v int64
		if decodeInt64(raw, &v) {
			f.value = v
		}
	}
	return f
}

// presentStrictField 用于本进程新建的合法记录（投票成功、首次计票、首次
// 执行）：按字段类型序列化 Go 值并填充，使“提交前校验”与“打开重放校验”
// 走同一份判定时不会把本进程新建的字段误判为缺失或类型不符。
func presentStrictField(kind strictFieldKind, value any) strictField {
	raw, err := json.Marshal(value)
	if err != nil {
		panic("govflow: cannot marshal strict field value: " + err.Error())
	}
	return captureStrictField(kind, raw)
}

func (f *strictField) stringValue() string {
	if v, ok := f.value.(string); ok {
		return v
	}
	return ""
}

func (f *strictField) boolValue() bool {
	if v, ok := f.value.(bool); ok {
		return v
	}
	return false
}

func (f *strictField) int64Value() int64 {
	if v, ok := f.value.(int64); ok {
		return v
	}
	return 0
}

// conforms 表示字段的原始写法是否满足声明的 JSON 类型。
// captureStrictField 只在类型合规时设置 value，故 value 非 nil 即合规。
func (f *strictField) conforms() bool {
	return f.value != nil
}

func strictKindName(k strictFieldKind) string {
	switch k {
	case strictString:
		return "string"
	case strictBoolean:
		return "boolean"
	case strictInteger:
		return "integer"
	default:
		return "value"
	}
}

// strictFieldSpec 把一个必填字段与其在保存格式中逐字一致的字段名配对。
type strictFieldSpec struct {
	name  string
	field *strictField
}

// validateStrictFields 按登记次序逐一判定必填字段，返回第一个不合要求字段的
// 带定位错误。多处字段同时有问题时报告次序与字段登记次序一致（票据为
// representative、weight、support、voted_at；计票为 for_weight、against_weight；
// 凭据为 executed_at），不会因整理提前变成没有记录位置的通用错误。
//
// location 是记录定位前缀，由各类记录按自己的业务含义拼好后传入
// （如 `voting proposal "gip-1" ballot 0`、`voting proposal "gip-1" tally`、
// `receipt 0 for proposal "gip-1"`），使错误既保留记录位置又带字段名与原因。
func validateStrictFields(location string, fields []strictFieldSpec) error {
	for _, fd := range fields {
		switch {
		case fd.field.raw == nil:
			return fmt.Errorf("%s field %q is missing", location, fd.name)
		case string(fd.field.raw) == "null":
			return fmt.Errorf("%s field %q is null", location, fd.name)
		case !fd.field.conforms():
			return fmt.Errorf("%s field %q has wrong type: want %s, got %s",
				location, fd.name, strictKindName(fd.field.kind), jsonValueType(fd.field.raw))
		}
	}
	return nil
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

// decodeInt64 判断原始 JSON 是否为落在有符号 64 位整数范围内的整数字面量：
// 必须是 JSON 数字（首字节为数字或 '-'），且能被 ParseInt 以 10 进制精确解析。
// 小数、指数、超界整数均返回 false；合规时把解析出的值写入 *out。
func decodeInt64(raw json.RawMessage, out *int64) bool {
	if jsonValueType(raw) != "number" {
		return false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return false
	}
	*out = v
	return true
}
