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

# 查询余额（未出现过的收款账户为 0）
govflow balances --state ./treasury.json
govflow balances --state ./treasury.json --account audits

# 查询提案状态（register 与投票两种来源都可查询）
govflow proposal  --state ./treasury.json --id gip-7
govflow proposals --state ./treasury.json

# 创建投票提案：成员名单与权重、可选委托、法定人数、投票窗口、时间锁与有序动作
govflow create-vote --state ./treasury.json --id gip-8 \
  --member alice:300 --member bob:200 --member carol:100 --member dave:400 \
  --delegate bob:alice --delegate carol:alice \
  --quorum 600 --start 100 --deadline 200 --timelock 300 \
  --action transfer:audits:100

# 按提案编号投票（时间由调用方通过 --now 提供）
# 只有最终代表本人可投票；bob、carol 已委托给 alice，由 alice 代表其权重
govflow vote  --state ./treasury.json --id gip-8 --voter alice  --choice for     --now 150
govflow vote  --state ./treasury.json --id gip-8 --voter dave   --choice against --now 150

# 截止时刻或之后计票；再次计票返回首次结论
govflow tally --state ./treasury.json --id gip-8 --now 200

# 通过后的提案直接交给 execute 执行，无须再 register；被拒绝的提案不能执行
govflow execute --state ./treasury.json --id gip-8 --now 300

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

### 投票提案

- 编号与成员编号必须非空，成员不得重复；权重为正整数；权重、法定人数与全部时间
  均限定在有符号 64 位整数范围内，成员总权重不得溢出；法定人数在 1 至总权重之间；
  时间满足 `0 <= 开始 < 截止 <= 时间锁`。任一非法条件整项拒绝，不落盘部分内容。
- 成员与委托的输入次序不影响相等性比较，动作仍按原文及顺序比较。同编号、全部内容
  相同的创建重试返回已有提案且不改变状态；内容不同报冲突。创建后规则与内容不可修改。
- 每人最多委托一名名单内成员，委托可继续转交；自委托、重复指定与循环一律拒绝。
  最终未再委托的人代表沿途全部成员投票，未委托者代表自己。查询展示每名成员的
  委托路径、最终代表与原始权重。
- 只有最终代表可以投赞成或反对：每位代表一张票，票重等于归到其名下的全部原始权重。
  首次投票只接受 `开始 <= now < 截止`；不在名单或已委托出去的人投票报明确错误。
  同一代表相同选择的重试始终返回首次记录；改投另一选择报冲突，不静默覆盖。
- 首次计票必须在截止时刻或之后，否则报“时间未到”且状态不变；未投权重不计入参与量。
  赞成与反对权重之和达到法定人数且赞成严格多于反对才通过；平票或参与不足均拒绝。
  计票只确定状态，不转账；再次计票返回首次结论；计票后拒绝新票，即使传入窗口内时间。
- 通过后的提案直接由 `execute` 执行，无须再登记；被拒绝的提案不能执行，
  已执行提案不退回通过。`register` 与投票提案共用编号：任何一方都不能覆盖另一方
  或绕过投票结论。
- 查询（文本与 `--json`）展示逐票代表、票重、选择、首次投票时间，以及汇总权重与
  首次计票时间。创建、投票、计票在多线程/多进程并发及重开后保持一致：成功重试不
  追加记录，失败不留部分变化。

### 幂等性与并发

- 一项提案只允许成功一次。再次执行即使传入不同时间，也返回首次成功凭据，
  不重新检查资格或扣款；重试不追加凭据。
- 凭据包含提案编号、首次执行时间、每项动作原文、资金库与收款账户变动前后的余额。
- 多线程或多进程同时操作同一状态文件时：同一提案只产生一份成功记录；
  不同提案依据实际已提交余额判断能否支出，查询不会看到部分转账。
  串行化由进程内互斥锁与 `flock` 文件锁共同保证。

### 故障与损坏处理

- 保存失败保留上次完整状态；执行中进程退出，重开只能看到执行前或执行后的完整状态。
- 已保存成功但尚未返回就退出，重试仍取得原凭据。
- 状态文件损坏（截断、非法 JSON、未知字段、固定字段名大小写不符、重复键、余额与凭据重放不一致、
  投票票据的票重/代表与名单及委托明细不一致、计票结论与票据重放不一致等）
  一律拒绝打开，不会重建或覆盖原文件；对损坏文件执行 init 同样被拒绝。
  固定字段名必须与保存格式逐字一致：把 `treasury` 写成 `Treasury`、把票据里的
  `support` 写成 `Support`、或在同一条记录里同时放入 `state` 与 `State`
  （即使两个值相同或恰好能通过余额/票据/计票核对）都拒绝整个文件；账户名与提案编号
  是业务数据而非字段名，`Audit` 与 `audit`、`GIP-1` 与 `gip-1` 始终是两个不同
  账户/编号，不拒绝、不合并、不改写。字段名按 JSON 解读后的字符串判断，合法转义拼写
  与普通拼写等价：单独出现正常识别，同一对象出现两次仍属重复键。错误信息指出问题字段
  及所在记录，命令行以状态码 1 失败并把原因写到标准错误。
  旧版本写出的、不含投票提案字段的状态文件继续可读；旧提案不补造投票记录。
- 提交采用临时文件、fsync、原子改名加目录 fsync；不同状态文件的资金与执行记录互不影响。

## 技术方向

dao, governance, voting, proposal, multisig, treasury-management, reputation-system

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
