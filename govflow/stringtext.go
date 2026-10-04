package govflow

import (
	"fmt"
	"unicode/utf8"
)

// 本文件是状态文件 JSON 字符串文本的原文校验。
//
// 提案编号、成员编号、收款账户与动作原文都有精确匹配的含义：读取功能必须
// 保留可信文本，遇到无法正确表示字符的内容就明确拒绝，绝不能悄悄改写。
// encoding/json 解码字符串时会把两类内容静默改写成替换字符 U+FFFD：
//   - 字符串中的非法 UTF-8 字节序列；
//   - \uXXXX 转义中未按“高代理项紧跟低代理项”组成有效字符的代理项。
//
// 若账户名和对应动作原文一起被这样改写，余额与凭据仍可能通过校验，查询却
// 展示了另一个账户名。因此 loadLocked 在结构扫描与解码之前先按原文逐字节
// 校验全部 JSON 字符串（对象键与字段值适用同一规则），发现问题即把整份
// 文件判为损坏——即使坏字符串位于本次没有查询的另一项提案中。
//
// 合法文本不受影响：中文、表情等有效 UTF-8，以及由合法高低代理项转义表示
// 的字符照常读取；同一字符直接写出或采用合法转义是同一文本，不影响既有
// 编号匹配与重复键判断。用户明确写出的 U+FFFD 本身是合法字符，不能仅凭
// 读取结果含有它就认定损坏；经过反斜杠转义的 Unicode 转义字样（如
// \\uD800 表示的 6 个普通字符）是普通文本，不参与代理项配对判定。

// checkStateStrings 逐字节扫描状态文件中的全部 JSON 字符串字面量：
//   - 字符串内出现非法 UTF-8 字节序列：报字节编码非法，位置是从文件开头
//     按零计数的第一个非法字节；
//   - 高代理项转义（D800–DBFF）没有紧跟低代理项转义（DC00–DFFF），或低
//     代理项转义未处在合法高代理项之后：报代理项未配对，位置指向该转义
//     开始的反斜杠。
//
// 扫描只识别字符串边界与转义结构，不替代语法检查：未终止的字符串、非法
// 转义字符、非法十六进制等语法问题留给后续解码器按既有规则报告。
func checkStateStrings(raw []byte) error {
	for i := 0; i < len(raw); {
		if raw[i] != '"' {
			i++
			continue
		}
		next, closed, err := scanJSONString(raw, i+1)
		if err != nil {
			return err
		}
		if !closed {
			// 未终止的字符串：语法错误由后续解码器报告，此处不再深入。
			return nil
		}
		i = next
	}
	return nil
}

// scanJSONString 从开引号之后的位置扫描一个字符串字面量，校验 UTF-8 字节
// 与代理项配对。closed 为 true 时 next 是闭引号之后的位置；未遇到闭引号
// （字符串被截断）时 closed 为 false，语法问题交给解码器报告。
func scanJSONString(raw []byte, i int) (next int, closed bool, err error) {
	for i < len(raw) {
		switch b := raw[i]; {
		case b == '"':
			return i + 1, true, nil
		case b == '\\':
			backslash := i
			if i+1 >= len(raw) {
				return 0, false, nil // 转义被截断：语法错误由解码器报告
			}
			if raw[i+1] != 'u' {
				// 其它转义整体跳过；\\ 之后的 uD800 等字样是普通文本，
				// 不是需要配对的字符转义。
				i += 2
				continue
			}
			if i+6 > len(raw) {
				return 0, false, nil // 转义被截断：语法错误由解码器报告
			}
			unit, ok := hex4(raw[i+2 : i+6])
			if !ok {
				return 0, false, nil // 非法十六进制：语法错误由解码器报告
			}
			switch {
			case isHighSurrogate(unit):
				// 高代理项必须紧跟一个 \uDC00–\uDFFF 低代理项转义，
				// 次序颠倒、跟错字符或落到字符串末尾都算未配对。
				if i+12 <= len(raw) && raw[i+6] == '\\' && raw[i+7] == 'u' {
					if low, ok := hex4(raw[i+8 : i+12]); ok && isLowSurrogate(low) {
						i += 12
						continue
					}
				}
				return 0, false, unpairedSurrogateError(unit, backslash)
			case isLowSurrogate(unit):
				return 0, false, unpairedSurrogateError(unit, backslash)
			default:
				i += 6
			}
		case b < 0x80:
			i++
		default:
			// 明确写出的 U+FFFD 是合法字符：DecodeRune 对它返回
			// (RuneError, 3)，只有 size == 1 才是真正的非法字节。
			r, size := utf8.DecodeRune(raw[i:])
			if r == utf8.RuneError && size == 1 {
				return 0, false, fmt.Errorf("invalid UTF-8 byte 0x%02X in JSON string at byte offset %d", b, i)
			}
			i += size
		}
	}
	return 0, false, nil
}

// unpairedSurrogateError 报告一个未配对的代理项转义；offset 指向该转义
// 开始的反斜杠（从文件开头按零计数）。
func unpairedSurrogateError(unit uint16, offset int) error {
	return fmt.Errorf("unpaired surrogate escape \\u%04X in JSON string at byte offset %d", unit, offset)
}

// hex4 解析 4 位十六进制数字为一个 UTF-16 代码单元。
func hex4(b []byte) (uint16, bool) {
	var v uint16
	for _, c := range b {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint16(c-'A') + 10
		default:
			return 0, false
		}
	}
	return v, true
}

func isHighSurrogate(unit uint16) bool { return unit >= 0xD800 && unit <= 0xDBFF }
func isLowSurrogate(unit uint16) bool  { return unit >= 0xDC00 && unit <= 0xDFFF }
