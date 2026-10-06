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

- `--registry`：登记文件路径。文件不存在时在首次登记时创建。路径也可以是指向已有登记文件的符号链接：登记结果保存到链接指向的文件（相对目标以链接所在目录为基准），链接本身保持原样，目标文件保留原有权限；链接目标不存在或链接成环时，本次操作被拒绝，不会创建目标文件或改动链接。
- `--batch`：批次编号，在同一登记文件内唯一。
- `--product`：产品编号。
- `--quantity`：数量，仅接受 `0`–`9` 组成的十进制正整数，允许前导零（如 `000120` 保存为 `120`），按整数值保存与比对，最大为 `9223372036854775807`。零、负数、小数、非 ASCII 数字和超范围值一律拒绝。
- `--unit`：计量单位。

`--batch`、`--product`、`--unit` 会先去掉首尾空白，去掉后不能为空；内部字符（含内部空白）保留，大小写不同视为不同值（`B1` 与 `b1` 是两个批次）。

这三个文本值必须是**合法 UTF-8**：含非法字节（即使首尾有空白包裹）时本次登记被拒绝，错误指出对应参数（如 `--batch`）编码无效，不创建或改写登记文件。合法 Unicode 文本均可使用，包括中文、表情符号和用户确实输入的替换字符 `�`（U+FFFD）——只凭出现该字符不会被拒绝，编号也不限于 ASCII。系统绝不会把非法字节静默替换成 `�`，以免不同输入被当成同一批次或引发原本不存在的重复冲突。

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

以下已有文件会被**明确拒绝**，绝不会当作空登记表覆盖：空文件或纯空白、内容无法按上述格式读取、`version` 不受支持、同一批次编号出现多条记录。根对象必须恰好包含 `version`（整数 `1`）和 `batches`（数组，可为空），每条批次记录必须恰好包含 `batch`、`product`、`quantity`、`unit` 四个字段；缺少字段、字段为 `null`、类型不符、出现多余字段或大小写变体（如 `Version`、`Batch`）、同一字段重复出现（含 `\u` 转义写出的同名，即使两个值相同）同样拒绝。三个文本字段含非法 UTF-8 字节或孤立的代理转义（如 `\ud800`）也会拒绝——不会读入被替换成 `�` 的内容再保存其他新批次。拒绝时错误指出文件路径、具体字段和原因；批次记录内的错误同时指出从 1 开始的记录位置，批次编号自身合法且能唯一确定时一并给出，绝不使用替换后的值指认批次。

## 整份到货清单导入：batch-import

一次提交整份清单，把多个批次登记到同一份登记文件：

```bash
govflow batch-import --registry batches.json --input arrivals.json
```

- `--registry`：登记文件路径，规则与 `batch-register` 完全相同；文件不存在时可在导入成功后创建。
- `--input`：清单文件路径，始终**只读**，不会被创建或修改。

### 输入文件

输入文件必须是一个**非空 JSON 数组**，每个元素是只含 `batch`、`product`、`quantity`、`unit` 四个必填字段的 JSON 对象：

```json
[
  {"batch": "B-001", "product": "P-7", "quantity": 120, "unit": "kg"},
  {"batch": "B-002", "product": "P-8", "quantity": 1, "unit": "box"}
]
```

- 三个文本字段按 `batch-register` 的规则规范化：去掉首尾空白后不能为空，内部字符（含内部空白）和大小写保留。它们还必须是合法 UTF-8：任一记录的任一字段出现非法字节或孤立代理转义，整份清单都失败——即使前面的记录合法也不写入，清单文件始终只读。错误指出清单路径、从 1 开始的记录位置和字段；仅当批次编号自身合法且能唯一确定时才显示编号，不会用替换后的 `�` 值指认批次。用户确实输入的 `�`（U+FFFD）、中文、表情以及非 ASCII 编号都正常接受，合法 JSON 转义（如 `中`）与直接书写同一字符表示相同文本。
- `quantity` 必须是 **JSON 整数**（如 `120`），范围为 1 到 9223372036854775807；字符串（`"120"`）、小数（`1.0`、`1.5`）、指数形式（`1e3`）、布尔、`null`、零、负数、超范围值一律拒绝。
- 空文件、纯空白、空数组 `[]`、非数组内容、数组元素不是对象、缺失字段、多余字段、字段重复或类型不符都会被明确拒绝，错误指出清单中**从 1 开始的记录位置**（能确定批次时同时给出批次编号）。

### 全有或全无

只有清单中**所有记录**都能新增或确认重复时才保存结果：

- 批次编号已在登记文件中、或此前已在本次清单中出现时，规范化后的产品、数量、单位必须**全部一致**，该条记录确认为重复。
- 任一字段不同则整份清单失败，没有任何新批次写入登记文件；错误指出记录位置（从 1 开始）、批次编号和不一致字段，以及冲突对象是登记文件中的记录还是清单中的更早记录及其位置。例如清单第 1、3 条同为 `B1` 但数量不同时：

```text
govflow: batch-import: manifest record 3 (batch "B1") conflicts with manifest record 1 on field(s): quantity; the whole manifest is rejected
```

- 后面的记录不能覆盖前面的记录。原登记文件没有 `B1` 时，两条完全相同的 `B1` 只新增一次；若第二条数量不同，第一条也不会留下。

### 成功输出

退出码为 0，标准输出只写一个 JSON 对象，`results` 数组按清单原顺序给出每条记录规范化后的四个字段和 `status`：

```json
{"results":[{"batch":"B-001","product":"P-7","quantity":120,"unit":"kg","status":"created"},{"batch":"B-001","product":"P-7","quantity":120,"unit":"kg","status":"duplicate"}]}
```

首次新增的记录标为 `created`，已存在或此前在本次清单中新增的相同记录标为 `duplicate`。已有记录的内容和顺序保持不变，新批次按首次出现的顺序追加。如果全部是重复记录，登记文件的**字节内容和修改时间都保持不变**。

读取任一文件失败、记录非法、发生冲突或保存失败时退出码非零，原因写入标准错误，标准输出不出现成功结果；已有登记文件保持原样，原本不存在则不留下登记文件。

## 通过 Go 库导入清单

`batch-import` 命令的全部能力也以 Go 库形式开放，包路径为 `github.com/gzhysuiioo/govflow-proposals/govflow/batchreg`。接入方按固定顺序组合四个公开入口：

1. `os.ReadFile` 读出清单字节，交给 `batchreg.ParseManifest(data)` 解析为 `[]batchreg.Input`——清单文件始终**只读**，库不会创建或修改它。
2. `batchreg.Load(registryPath)` 载入登记文件。文件不存在时返回空登记表且 `existed == false`，这不是错误；登记文件将在保存成功后创建。
3. `batchreg.Import(reg, inputs)` 在内存中应用整份清单，返回 `[]batchreg.ImportResult`。这只是**内存处理结果**：返回成功不代表已落盘。
4. 至少有一条 `Created` 为 `true` 时调用 `batchreg.Save(registryPath, reg)` 原子写入；保存成功才意味着新增批次真正登记完成。

### 完整的最小示例

下面是一个可直接运行的最小程序（`go.mod` 中 require 本模块即可），它把 `arrivals.json` 导入 `batches.json`，并严格区分内存处理结果与真正保存成功的结果：

```go
// 最小示例：把一份到货清单导入指定的本地登记文件。
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/gzhysuiioo/govflow-proposals/govflow/batchreg"
)

func main() {
	registryPath := "batches.json" // 登记文件；不存在时在保存成功后创建
	manifestPath := "arrivals.json"

	// 1. 清单文件始终只读：只读出字节，绝不写回。
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		log.Fatalf("cannot read manifest: %v", err)
	}
	inputs, err := batchreg.ParseManifest(data)
	if err != nil {
		// *batchreg.ManifestRecordError：从 1 开始的记录位置、可确定的批次编号、原因
		log.Fatalf("invalid manifest %s: %v", manifestPath, err)
	}

	// 2. 载入登记文件；不存在时得到空登记表，existed 为 false。
	reg, _, err := batchreg.Load(registryPath)
	if err != nil {
		log.Fatalf("cannot load registry: %v", err)
	}

	// 3. Import 只返回内存处理结果：整份被拒绝时 reg 不变；
	//    成功时 reg 已包含新批次，但磁盘文件尚未写入。
	results, err := batchreg.Import(reg, inputs)
	if err != nil {
		// *batchreg.ManifestConflictError：记录位置、批次编号、冲突字段与冲突来源
		log.Fatalf("manifest rejected, registry unchanged: %v", err)
	}

	// 4. 只有出现新增记录才保存；全部重复时登记文件的字节内容和
	//    修改时间必须保持不变，不能为了打印结果而再次保存。
	anyCreated := false
	for _, r := range results {
		if r.Created {
			anyCreated = true
		}
	}
	if anyCreated {
		if err := batchreg.Save(registryPath, reg); err != nil {
			// 保存失败：上面的 created 结果并未完成登记，磁盘文件
			// 未接受这些新批次（内存中的 reg 可能已经改变）。
			log.Fatalf("import NOT saved: %v", err)
		}
	}

	// 5. 走到这里，created 的记录才真正保存成功（或本来就已登记）。
	for _, r := range results {
		status := "duplicate"
		if r.Created {
			status = "created"
		}
		fmt.Printf("%s batch=%s product=%s quantity=%d unit=%s\n",
			status, r.Batch.Batch, r.Batch.Product, r.Batch.Quantity, r.Batch.Unit)
	}
	if anyCreated {
		fmt.Println("registry saved:", registryPath)
	} else {
		fmt.Println("all records already registered; registry file untouched")
	}
}
```

### 混合清单示例

登记文件 `batches.json` 已含 `B-001`，清单 `arrivals.json` 混合了三种记录——尚未登记的新批次 `B-002`、与登记文件完全相同的 `B-001`、以及在清单内再次出现的相同新批次 `B-002`：

```json
[
  {"batch": "B-002", "product": "P-8", "quantity": 30, "unit": "box"},
  {"batch": "B-001", "product": "P-7", "quantity": 120, "unit": "kg"},
  {"batch": "B-002", "product": "P-8", "quantity": 30, "unit": "box"}
]
```

运行结果（结果严格保持清单顺序，每条记录都能看出是新增还是重复确认）：

```text
created batch=B-002 product=P-8 quantity=30 unit=box
duplicate batch=B-001 product=P-7 quantity=120 unit=kg
duplicate batch=B-002 product=P-8 quantity=30 unit=box
registry saved: batches.json
```

保存后的登记文件：原有记录 `B-001` 及其位置原样保留，同一新编号 `B-002` 只保存一次、按首次出现的顺序追加在末尾：

```json
{
  "version": 1,
  "batches": [
    {"batch": "B-001", "product": "P-7", "quantity": 120, "unit": "kg"},
    {"batch": "B-002", "product": "P-8", "quantity": 30, "unit": "box"}
  ]
}
```

### 全部记录都已存在时

如果清单中每条记录都与登记文件中的记录完全一致（全部为 `duplicate`），示例中的 `anyCreated` 为 `false`，程序**不调用 `Save`**，直接打印结果：

```text
duplicate batch=B-001 product=P-7 quantity=120 unit=kg
duplicate batch=B-002 product=P-8 quantity=30 unit=box
all records already registered; registry file untouched
```

此时登记文件的字节内容和修改时间都必须保持不变——`Save` 会重写文件并刷新修改时间，因此绝不能为了打印结果或"保险起见"而再次保存。这也是重试一份已导入清单的安全方式。

### 文本与数量规则（按入口区分）

- **解析清单文件**（`ParseManifest`）：`batch`、`product`、`unit` 先去掉首尾空白，去掉后不能为空；`quantity` 必须是 **JSON 整数**（如 `120`），字符串（`"120"`）、小数（`1.0`）、指数形式（`1e3`）一律拒绝。
- **通过 Go 库直接构造 `batchreg.Input` 提交**（`Import` / `Register`）：三个文本值**按原样**保存和比较，首尾空白不会被去掉；只有空白的字符串（如 `" "`）是现有库接受的非空值，只有空字符串 `""` 才被拒绝。命令行入口之所以有去空白行为，是因为它在构造 `Input` 之前先调用了 `batchreg.NormalizeField`——直接构造时如需同样效果，请自行调用。
- 两种方式都**保留大小写和内部字符**（`B1` 与 `b1` 是两个批次），都拒绝非法 UTF-8 字节；用户真实输入的替换字符 `�`（U+FFFD）是合法文本，可以正常使用。
- 数量的有效范围相同，都是 1 到 9223372036854775807（`batchreg.MaxQuantity`）。

### 失败语义

- **记录非法**：任一记录非法（文本为空或非法 UTF-8、数量越界等），整份导入被拒绝，错误为 `*batchreg.ManifestRecordError`，指出**从 1 开始**的记录位置（`Position`）、可确定的批次编号（`Batch`，批次编号自身缺失或非法时为空，绝不使用替换后的值）和原因（`Reason`）。
- **同编号内容冲突**：任一记录与登记文件中的记录、或与清单内更早的同编号记录在 `product`、`quantity`、`unit` 上任一项不同，整份导入被拒绝，错误为 `*batchreg.ManifestConflictError`，指出记录位置（`Position`）、批次编号（`Batch`）、全部冲突字段（`Fields`）和冲突来源（`Source` 为 `"registry"` 或 `"manifest"`；为 `"manifest"` 时 `PrevPos` 给出清单内更早记录的位置）。
- 以上两种失败都**没有部分成功**：`Import` 返回错误时 `reg` 保持原样，不会展示部分结果，更不会继续保存。调用方可以用 `errors.As` 取出上述具体错误类型。
- **保存失败**：`Save` 返回的错误一定包含登记文件路径与原因（如 `cannot save registry "batches.json": ...`）。此时**不能把先前 `Import` 返回的 `created` 结果当作已完成登记**：内存中的 `reg` 可能已经包含这些新批次，但磁盘文件并未接受它们——已有文件保持原字节，原本不存在的文件不会留下（命令行入口会在此时清理首次保存产生的半成品文件，库调用方如需同样行为请自行删除）。要确认登记完成，只能以 `Save` 成功返回为准。

## 技术方向

dao, governance, voting, proposal, multisig, treasury-management, reputation-system

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
