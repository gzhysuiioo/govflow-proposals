# DAO 治理提案与执行平台

## 用途

提案生命周期、投票与计票规则、委托与声誉、资金库支出与多签执行、时间锁与回滚、治理审计。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `govflow/`，命令入口位于 `cmd/govflow/`。

```bash
go run ./cmd/govflow demo
go run ./cmd/govflow version
go run ./cmd/govflow help
go test ./...
```

## 供应链批次登记：batch-register

把供应链中的一个批次登记到指定的本地 JSON 登记文件。一次调用登记一个批次：

```bash
govflow batch-register \
  --registry batches.json \
  --batch B-001 \
  --product P-7 \
  --quantity 120 \
  --unit kg
```

- `--registry`：登记文件路径。文件不存在时在首次登记时创建。
- `--batch`：批次编号，在同一登记文件内唯一。
- `--product`：产品编号。
- `--quantity`：数量，仅接受 `0`–`9` 组成的十进制正整数，允许前导零（如 `000120` 保存为 `120`），按整数值保存与比对，最大为 `9223372036854775807`。零、负数、小数、非 ASCII 数字和超范围值一律拒绝。
- `--unit`：计量单位。

`--batch`、`--product`、`--unit` 会先去掉首尾空白，去掉后不能为空；内部字符（含内部空白）保留，大小写不同视为不同值（`B1` 与 `b1` 是两个批次）。

### 成功输出

标准输出只输出一个 JSON 对象，退出码为 0：

```json
{"batch":"B-001","product":"P-7","quantity":120,"unit":"kg","status":"created"}
```

`status` 为 `created`（新增）或 `duplicate`（重复登记）。

- 同一编号再次提交，且规范化后的产品编号、数量、单位与已存记录**完全一致**时，返回已有记录和 `"duplicate"`，不新增记录、不改写文件——可以安全重试。
- 同一编号但产品、数量、单位中**任意一项不同**时，登记被拒绝，错误信息指出批次编号和不一致的字段，原记录和其他批次都不受影响。重复登记不能用于修改数量或产品归属，例如：

```text
govflow: batch "B-001" is already registered with conflicting field(s): quantity; the existing record cannot be overwritten
```

失败时退出码非零，原因写入标准错误，标准输出不出现任何成功结果。缺少必需参数或参数非法时，不会创建或修改登记文件。

## 批量导入：batch-import

一次提交整份清单，把输入文件里的多个批次登记到同一份登记文件：

```bash
govflow batch-import \
  --registry batches.json \
  --input incoming.json
```

- `--registry`：登记文件路径。文件不存在时在成功导入后创建。
- `--input`：输入文件，始终只读，程序不会写入或修改它。

输入文件是一个**非空 JSON 数组**，每个元素只含 `batch`、`product`、`quantity`、`unit` 四个必填字段，多字段、缺字段、元素不是对象或数组为空都明确拒绝。三个文本字段去掉首尾空白后不能为空，内部字符和大小写保留；`quantity` 必须是 JSON 整数，范围为 `1` 到 `9223372036854775807`，不接受字符串、小数或指数形式。

### 原子性

只有清单中**所有**记录都能够登记或确认为重复时才保存结果；任何一条被拒绝（记录非法或字段冲突）都不登记本次清单里的新批次，已有登记文件保持原样，原本不存在则不留下登记文件。

同一批次编号既可能已在登记文件中，也可能在清单前面的记录中出现。规范化后的产品、数量和单位全部一致时确认重复；任一字段不同则整份清单失败，错误指出清单中从 1 开始的记录位置、批次编号和不一致的字段。不能靠后面的记录覆盖前面的记录。例如原登记文件没有 B1 时，两条完全相同的 B1 只新增一次；若第二条数量不同，第一条也不能留下。

### 成功输出

退出码为 0，标准输出只写一个 JSON 对象，`results` 数组按清单原顺序给出每条记录规范化后的四个字段和 `status`：

```json
{"results":[{"batch":"B-001","product":"P-7","quantity":120,"unit":"kg","status":"created"},{"batch":"B-002","product":"P-8","quantity":5,"unit":"box","status":"duplicate"}]}
```

- `status` 为 `created`（首次新增）或 `duplicate`（已存在或此前在本次清单中新增的相同记录）。
- 已有记录的内容和顺序保持不变，新批次按首次出现的顺序追加。
- 如果全部是重复记录，登记文件的字节和修改时间都保持不变。

### 登记文件格式（公开）

登记文件是人类可检查的 UTF-8 JSON：

```json
{
  "version": 1,
  "batches": [
    {"batch": "B-001", "product": "P-7", "quantity": 120, "unit": "kg"}
  ]
}
```

保存采用临时文件加原子替换，写入失败时已有批次仍然可用。不同登记文件各自管理编号，向已有文件登记新批次时保留其中其他批次的完整信息。

以下已有文件会被**明确拒绝**，绝不会当作空登记表覆盖：空文件或纯空白、内容无法按上述格式读取、`version` 不受支持、同一批次编号出现多条记录。

## 技术方向

dao, governance, voting, proposal, multisig, treasury-management, reputation-system

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
