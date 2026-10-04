package govflow

import (
	"fmt"
	"unicode/utf8"
)

// 本文件在状态文件交给 encoding/json 之前逐字节扫描其中的每一个 JSON
// 字符串（对象键与字段值同等适用），保证读取路径拿到的文本可信：
//
//   - 字符串中出现非法 UTF-8 字节序列（残缺的多字节序列、续字节缺失、
//     越界编码、WTF-8 形式写出的代理码位等）一律判整份文件损坏；
//   - \u Unicode 转义中的高代理项（U+D800–U+DBFF）与低代理项
//     （U+DC00–U+DFFF）必须按“高后立即低”的次序组成一对：
//     孤立高代理、孤立低代理、高代理后没有紧跟合法低代理转义都判损坏。
//
// encoding/json 默认把这两类内容悄悄改写成替换字符 U+FFFD 且不报错：
// 一旦账户名、提案编号、成员编号或动作原文被改写，余额与凭据仍可能重放
// 一致，查询却展示出另一个名字。扫描先于解码执行，被改写的字符串因此
// 不可能参与编号匹配、重复键判断或凭据重放——坏字符串即使出现在本次
// 没有查询的另一项提案中，整份文件也会被拒绝。
//
// 合法内容不被误伤：
//   - 中文、表情等一切合法 UTF-8 文本照常读取（包括用户明确写出的
//     U+FFFD 本身：无论直接写出 EF BF BD 还是用 `\uFFFD` 转义，都是合法
//     字符，不能仅凭读取结果含有它就认定损坏）；
//   - 一对合法的高、低代理项转义（如 😀）照常按其表示的字符
//     读取，与直接写出该字符等价，不影响编号匹配与重复键判断；
//   - 反斜杠自身被转义后出现的 “\uD800” 字样（源码拼写 "\\uD800"）是
//     普通文本：第一个反斜杠配对第二个反斜杠，uD800 只是字面字符，
//     不参与代理项配对。
//
// 报错区分两类原因，并给出从文件开头按零计数的字节位置；未配对转义的
// 位置指向该转义开始的反斜杠。这里只报告字符层面的问题，不替代字段
// 校验、投票规则与凭据重放：字符合法绝不宽免其它损坏原因。

// checkStrictStrings 逐字节扫描整份 JSON 文档中的字符串内容。
// 发现非法 UTF-8 或未配对代理项转义时返回带原因与字节偏移的错误；
// 文档存在其它语法问题时不保证在此报错（交给后续结构扫描与解码器）。
func checkStrictStrings(raw []byte) error {
	n := len(raw)
	for i := 0; i < n; {
		if raw[i] != '"' {
			i++
			continue
		}
		// 进入一个 JSON 字符串（对象键或值），content 指向开头引号。
		i++ // 越过开头引号
		for i < n {
			c := raw[i]
			if c == '"' {
				i++ // 越过结尾引号
				break
			}
			if c == '\\' {
				bs := i // 转义起始反斜杠；代理项报错位置指向这里
				i++
				if i >= n {
					break // 反斜杠后即文件结尾：未终止字符串，交给解码器拒绝
				}
				if raw[i] != 'u' {
					// 单字符转义（" \ / b f n r t）原样跳过；
					// 非法转义字符不在字符校验范围内，由解码器报告。
					// 特别地，"\\uD800" 中第一个反斜杠与第二个反斜杠配对，
					// 随后的 uD800 只是普通文本，不需要代理项配对。
					i++
					continue
				}
				cp, ok := hexEscape4(raw, i+1) // i 指向 'u'，其后是 4 个十六进制位
				if !ok {
					// \u 后不足 4 个十六进制位是 JSON 语法错误，交给解码器；
					// 不把它解释成半个代理项，跳过 'u' 继续扫描其余内容。
					i++
					continue
				}
				switch {
				case cp >= 0xD800 && cp <= 0xDBFF:
					// 高代理项：必须立即紧跟一个低代理项转义才算合法。
					if _, paired := followingLowSurrogate(raw, bs+6); !paired {
						return fmt.Errorf("invalid Unicode surrogate in JSON string at byte offset %d: unpaired high surrogate escape \\u%04X (expected a following low surrogate escape \\uDC00-\\uDFFF)",
							bs, cp)
					}
					i = bs + 12 // 越过完整的一对转义（\uXXXX\uXXXX 共 12 字节）
				case cp >= 0xDC00 && cp <= 0xDFFF:
					// 低代理项没有前导高代理项，无法单独表示字符。
					return fmt.Errorf("invalid Unicode surrogate in JSON string at byte offset %d: unpaired low surrogate escape \\u%04X (no preceding high surrogate escape \\uD800-\\uDBFF)",
						bs, cp)
				default:
					i = bs + 6 // 越过完整的 \uXXXX 转义
				}
				continue
			}
			if c < 0x80 {
				// ASCII 字符（含未转义控制字符）原样跳过；
				// 未转义控制字符是 JSON 语法错误，由解码器报告。
				i++
				continue
			}
			// 非 ASCII 原始字节必须是一个完整合法的 UTF-8 编码。
			// DecodeRune 对非法首字节、截断序列、越界编码及 WTF-8 形式的
			// 代理码位（如 ED A0 80）都返回 RuneError 且 size==1；
			// 合法的 U+FFFD（EF BF BD）size==3，照常放行。
			_, size := utf8.DecodeRune(raw[i:])
			if size == 1 {
				return fmt.Errorf("invalid UTF-8 encoding in JSON string at byte offset %d: illegal byte 0x%02X",
					i, c)
			}
			i += size
		}
	}
	return nil
}

// hexEscape4 解析从 pos 开始的 4 个十六进制位为一个码位值；
// 位数不足或任一位不是十六进制字符时 ok 为 false。
func hexEscape4(raw []byte, pos int) (rune, bool) {
	if pos+4 > len(raw) {
		return 0, false
	}
	var value rune
	for k := 0; k < 4; k++ {
		c := raw[pos+k]
		var digit rune
		switch {
		case c >= '0' && c <= '9':
			digit = rune(c - '0')
		case c >= 'a' && c <= 'f':
			digit = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			digit = rune(c-'A') + 10
		default:
			return 0, false
		}
		value = value<<4 | digit
	}
	return value, true
}

// followingLowSurrogate 判断从 pos 开始是否紧跟一个低代理项转义
// \uDC00–\uDFFF（恰好 6 字节：反斜杠、u、4 个十六进制位）。
func followingLowSurrogate(raw []byte, pos int) (rune, bool) {
	if pos+6 > len(raw) || raw[pos] != '\\' || raw[pos+1] != 'u' {
		return 0, false
	}
	low, ok := hexEscape4(raw, pos+2)
	if !ok || low < 0xDC00 || low > 0xDFFF {
		return 0, false
	}
	return low, true
}
