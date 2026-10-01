# DAO 治理提案与执行平台

## 用途

提案生命周期、投票与计票规则、委托与声誉、资金库支出与多签执行、时间锁与回滚、治理审计。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `govflow/`，命令入口位于 `cmd/govflow/`。

```bash
go run ./cmd/govflow demo
go run ./cmd/govflow version
go test ./...
```

## 本地资金库

到期提案可以对本地资金库执行真实转账。状态持久化在单个 JSON 状态文件中，
余额、提案登记与执行凭据一起保存；退出后重新打开可继续查询与执行。
全部行为只依赖 Go 标准库，离线可复现。

### 命令

```bash
# 初始化资金库（只有新文件可以初始化，已有文件不会重置余额）
govflow init --state ./treasury.json --balance 10000

# 登记已通过提案：编号非空，保留时间锁与动作原文
# 同编号相同内容的重试幂等返回；内容不同报冲突
govflow register --state ./treasury.json \
  --id gip-7 --timelock 5000 \
  --action transfer:audits:25000 \
  --action transfer:legal:1000

# 按编号执行（当前时间由调用方通过 --now 提供）
# 只有 passed、时间已达时间锁、动作非空的提案可首次执行；成功后变为 executed
govflow execute --state ./treasury.json --id gip-7 --now 5001

# 创建投票提案：成员名单与权重、可选委托（from:to）、法定人数、
# 投票起止时间 [start, end)、执行时间锁与有序动作
govflow propose --state ./treasury.json --id gip-1 \
  --member alice:60 --member bob:40 --member carol:20 \
  --delegate carol:bob --delegate bob:alice \
  --quorum 80 --start 100 --end 200 --timelock 500 \
  --action transfer:audits:10

# 投票：仅最终代表可投，票重为归到其名下的全部原始权重
govflow vote --state ./treasury.json --id gip-1 --member alice --choice for --now 150

# 计票：首次计票须在截止时刻或之后；赞成反对权重之和达到法定人数
# 且赞成严格多于反对才通过，否则拒绝；再次计票返回首次结论
govflow tally --state ./treasury.json --id gip-1 --now 200

# 查询投票提案：成员委托路径、最终代表、原始权重；逐票代表、票重、
# 选择、首次投票时间；汇总与首次计票时间
govflow voting  --state ./treasury.json --id gip-1
govflow votings --state ./treasury.json

# 查询余额（未出现过的收款账户为 0）
govflow balances --state ./treasury.json
govflow balances --state ./treasury.json --account audits

# 查询提案状态
govflow proposal  --state ./treasury.json --id gip-7
govflow proposals --state ./treasury.json

# 查询执行凭据（按成功提交先后顺序）
govflow receipt  --state ./treasury.json --id gip-7
govflow receipts --state ./treasury.json

# 所有资金库命令支持 --json
govflow execute --state ./treasury.json --id gip-7 --now 5001 --json
```

命令失败时原因输出到 stderr，域错误退出码为 1，参数错误为 2。

### 动作与数值规则

- 动作只支持 `transfer:<账户>:<金额>`；收款账户不能为空或包含冒号。
- 金额只接受十进制数字表示的正整数，且与全部余额一起限定在有符号 64 位整数范围内。
- 执行按动作原顺序从资金库扣款并增加对应账户余额，同一账户可以连续收款。
- 整项提案先完整预演再一次性提交：编号不存在、时间未到、状态不符、动作格式错误、
  余额不足或金额计算溢出都返回明确原因，不产生部分转账，失败不消耗执行机会。

### 幂等性与并发

- 一项提案只允许成功一次。再次执行即使传入不同时间，也返回首次成功凭据，
  不重新检查资格或扣款；重试不追加凭据。
- 凭据包含提案编号、首次执行时间、每项动作原文、资金库与收款账户变动前后的余额。
- 多线程或多进程同时操作同一状态文件时：同一提案只产生一份成功记录；
  不同提案依据实际已提交余额判断能否支出，查询不会看到部分转账。
  串行化由进程内互斥锁与 `flock` 文件锁共同保证。

### 投票幂等性与一致性

- 创建投票提案：相同编号且全部内容相同（成员与委托按集合比较、与输入次序无关，
  动作按原文及顺序比较）的重试返回已有提案且不改变当前状态；内容不同报冲突。
- 投票：同一代表相同选择的重试始终返回首次记录（含首次投票时间），改投另一选择
  报冲突；计票后拒绝新票，即使传入窗口内时间。
- 计票：首次计票须在截止时刻或之后，否则报时间未到且状态不变；再次计票返回首次结论。
- 保存的票重、代表归属或计票结论与名单及明细不一致时拒绝打开，保留原文件。

### 故障与损坏处理

- 保存失败保留上次完整状态；执行中进程退出，重开只能看到执行前或执行后的完整状态。
- 已保存成功但尚未返回就退出，重试仍取得原凭据。
- 状态文件损坏（截断、非法 JSON、未知字段、重复键、余额与凭据重放不一致等）
  一律拒绝打开，不会重建或覆盖原文件；对损坏文件执行 init 同样被拒绝。
- 提交采用临时文件、fsync、原子改名加目录 fsync；不同状态文件的资金与执行记录互不影响。

## 技术方向

dao, governance, voting, proposal, multisig, treasury-management, reputation-system

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
