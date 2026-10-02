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

## batch-register：批次登记

把供应链中的一个批次登记到指定的本地文件，一次操作登记一个批次。

```bash
go run ./cmd/govflow batch-register \
  --registry ./registry.jsonl \
  --batch B001 --product P100 --quantity 100 --unit 箱
# {"batch":"B001","product":"P100","quantity":100,"unit":"箱","status":"new"}
```

- `--registry`、`--batch`、`--product`、`--quantity`、`--unit` 均为必填，可按任意顺序给出。
- 编号、产品编号、单位去掉首尾空白后不能为空，内部字符保留，大小写不同视为不同值。
- 数量只接受 0-9 组成的十进制正整数（可有前导零），按整数值保存和比对，范围不超过有符号 64 位整数最大值；零、负数、小数、越界均被拒绝。
- 同一编号再次提交：规范化后的产品编号、数量、单位全部一致时作为成功的重复登记返回已有记录（`status:"duplicate"`），不增加记录、不改写文件；任一字段不同则拒绝，错误中指出批次编号与不一致字段，原记录保持不变。
- 成功时标准输出只有一个 JSON 对象，退出码为 0；失败时退出码非零，标准错误说明原因，标准输出无内容。
- 缺少参数或输入非法时不创建、不改变登记文件。文件不存在时可开始登记；已有文件为空、内容无法按公开格式读取，或同一编号存在多条记录时，明确拒绝，不会覆盖。

### 登记文件格式（公开格式）

每行一个 JSON 对象：

```json
{"batch":"B001","product":"P100","quantity":100,"unit":"箱"}
```

- `batch`：批次编号，文件内唯一；首尾空白会被去掉；大小写敏感。
- `product`：产品编号；首尾空白会被去掉。
- `quantity`：十进制正整数，按整数值保存。
- `unit`：计量单位；首尾空白会被去掉。

重复登记（编号相同且产品、数量、单位一致）返回 `status:"duplicate"` 且不改写文件；编号冲突（编号相同但任一字段不同）会被拒绝并指出不一致字段。`govflow batch-register -h` 给出完整使用说明。

## 技术方向

dao, governance, voting, proposal, multisig, treasury-management, reputation-system

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
