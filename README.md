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

- `--registry`：登记文件路径。文件不存在时在首次登记时创建。路径也可以是指向已有登记文件的符号链接：登记结果保存到链接指向的文件（相对目标以链接所在目录为基准），链接本身保持原样，目标文件保留原有权限；链接目标不存在或链接成环时，本次操作被拒绝，不会创建目标文件或改动链接。符号链接也可以出现在路径的**目录**部分，并与 `..` 混用：读取和保存按文件系统的实际指向解析同一路径，例如 `work/alias` 是指向 `store/child` 的目录链接时，`work/alias/../batches.json` 指的是 `store/batches.json`，新增批次保存进该真实文件，绝不会因为链接名后面的 `..` 而改写链接上一级目录里的同名文件（`work/batches.json` 即使存在也与本次登记无关）；保存只在真实目标文件所在目录创建并替换临时文件，因此即使 `work` 目录只允许读取和进入、不允许创建文件，登记依然成功，也不会在 `work` 下留下登记文件、临时文件或新目录。真实目标目录不可写时按保存失败拒绝，错误指出用户传入的登记路径和原因，原文件字节与修改时间保持不变。
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

## 通过 Go 库导入到货清单：batchreg

`govflow/batchreg` 包把命令行 `batch-import` 的同一套能力作为公开入口提供，可在自己的 Go 程序里把一份清单导入指定的本地登记文件。导入路径为 `github.com/gzhysuiioo/govflow-proposals/govflow/batchreg`（仅用标准库）：

1. `batchreg.ParseManifest(data []byte) ([]batchreg.Input, error)`：解析清单文件字节。清单文件始终**只读**：由调用方自己读入，库只校验和转换这些字节，绝不创建或改写清单文件。
2. `batchreg.Load(path string) (reg *batchreg.Registry, existed bool, err error)`：读取前文公开格式的登记文件。文件不存在不是错误：返回一个空登记表和 `existed == false`；已有但无法按公开格式读取的文件按错误拒绝，绝不覆盖。
3. `batchreg.Import(reg *batchreg.Registry, inputs []batchreg.Input) (results []batchreg.ImportResult, err error)`：在**内存中**把清单合并进 `reg`，返回按清单顺序排列的逐条结果。
4. `batchreg.Save(path string, reg *batchreg.Registry) error`：临时文件加原子替换，把登记表写入登记文件；此前不存在的文件在**保存成功后**创建（权限 0644），符号链接路径的规则与命令行章节完全相同。

`ImportResult` 含合并后的 `Batch`（四个字段）和 `Created bool`：`true` 表示该编号是本次新出现并追加的，`false` 表示重复确认。**`Import` 的返回值只是内存处理结果，不是登记完成的凭据**：新批次此刻只存在于内存中的 `reg`；只有随后 `Save` 成功返回，它们才真正被登记文件接受。

### 完整最小示例

```go
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/gzhysuiioo/govflow-proposals/govflow/batchreg"
)

func main() {
	dir, err := os.MkdirTemp("", "arrivals-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	manifestPath := filepath.Join(dir, "arrivals.json")
	registryPath := filepath.Join(dir, "batches.json")

	// A registry that already holds B-100. The seed file is only needed for
	// the demo; with no registry at all, Load returns an empty registry and
	// Save creates the file.
	if err := os.WriteFile(registryPath, []byte(`{
  "version": 1,
  "batches": [
    {"batch": "B-100", "product": "P-7", "quantity": 120, "unit": "kg"}
  ]
}`), 0o644); err != nil {
		log.Fatal(err)
	}

	// Read-only manifest: a new batch (padded with whitespace), a batch
	// identical to the registered record, and the same new batch repeated.
	manifestJSON := `[
  {"batch": " B-200 ", "product": " P-8 ", "quantity": 30, "unit": " box "},
  {"batch": "B-100", "product": "P-7", "quantity": 120, "unit": "kg"},
  {"batch": "B-200", "product": "P-8", "quantity": 30, "unit": "box"}
]`
	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0o644); err != nil {
		log.Fatal(err)
	}

	// 1) Parse the manifest. ParseManifest trims the three text fields and
	//    validates every record; the manifest bytes are never written.
	inputs, err := batchreg.ParseManifest(mustReadFile(manifestPath))
	if err != nil {
		log.Fatalf("invalid manifest %q: %v", manifestPath, err)
	}

	// 2) Load the registry: *Registry plus whether the file existed.
	reg, existed, err := batchreg.Load(registryPath)
	if err != nil {
		log.Fatal(err)
	}

	// 3) Import into memory. On error the whole manifest was rejected and reg
	//    is untouched; results is nil.
	results, err := batchreg.Import(reg, inputs)
	if err != nil {
		log.Fatalf("import rejected: %v", err)
	}

	// results is the IN-MEMORY outcome only — nothing has been saved yet.
	anyCreated := false
	for i, r := range results {
		status := "duplicate"
		if r.Created {
			status = "created"
			anyCreated = true
		}
		fmt.Printf("result %d: %s / %s / %d / %s -> %s\n",
			i+1, r.Batch.Batch, r.Batch.Product, r.Batch.Quantity, r.Batch.Unit, status)
	}

	// 4) Persist. Save only when at least one record was created: an
	//    all-duplicate import must leave the file's bytes and mtime untouched.
	if anyCreated {
		if err := batchreg.Save(registryPath, reg); err != nil {
			// reg already carries the new batches in memory, but the disk file
			// does NOT — report the path and the cause, never "imported".
			log.Fatalf("registry %q not saved: %v", registryPath, err)
		}
		fmt.Printf("saved %s (file existed beforehand: %v)\n", registryPath, existed)
	} else {
		fmt.Println("every record already existed; the registry file was not touched")
	}

	// Re-load to show what the disk accepted.
	saved, _, err := batchreg.Load(registryPath)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("registry on disk now holds %d record(s)\n", len(saved.Batches))
}

func mustReadFile(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("cannot read manifest %q: %v", path, err)
	}
	return data
}
```

运行输出（`/tmp/arrivals-*` 是程序随机生成的临时目录，每次不同）：

```text
result 1: B-200 / P-8 / 30 / box -> created
result 2: B-100 / P-7 / 120 / kg -> duplicate
result 3: B-200 / P-8 / 30 / box -> duplicate
saved /tmp/arrivals-2218460593/batches.json (file existed beforehand: true)
registry on disk now holds 2 record(s)
```

从输出可以逐条读出：

- 第 1 条 `B-200` 尚未登记，标为 `created`；清单字段首尾的空白在解析时去掉，保存为 `B-200 / P-8 / 30 / box`。
- 第 2 条 `B-100` 与登记文件中的记录**完全一致**，标为 `duplicate`，是重复确认而不是新增。
- 第 3 条 `B-200` 与本清单第 1 条相同，同样标为 `duplicate`；同一新编号在整个清单中**只保存一次**。
- `results` 始终保持清单原顺序。保存后登记文件里原有 `B-100` 的内容和位置不变，`B-200` 只在末尾追加一次：

```json
{
  "version": 1,
  "batches": [
    {
      "batch": "B-100",
      "product": "P-7",
      "quantity": 120,
      "unit": "kg"
    },
    {
      "batch": "B-200",
      "product": "P-8",
      "quantity": 30,
      "unit": "box"
    }
  ]
}
```

### 全部记录都已存在且内容一致

把同一份清单（或只含 `B-100`、`B-200` 的清单）再导一次时，每条结果的 `Created` 都是 `false`。此时使用方式与上面示例相同：扫描结果，只有 `anyCreated == true` 才调用 `Save`；全重复时**不要**为了打印结果或“保险”而再保存一次——`Save` 每次都会重写文件，多余调用会平白改变修改时间。跳过 `Save` 时登记文件的字节内容和修改时间都保持不变，可用下面的断言确认：

```go
before, _ := os.ReadFile(registryPath)
infoBefore, _ := os.Stat(registryPath)

results, err := batchreg.Import(reg, inputs) // 全部 duplicate
// ... 打印 results 不需要触碰磁盘；anyCreated == false，因此不调用 Save

after, _ := os.ReadFile(registryPath)
infoAfter, _ := os.Stat(registryPath)
fmt.Println(string(before) == string(after), infoBefore.ModTime().Equal(infoAfter.ModTime())) // true true
```

重复确认是幂等的，可以安全重试；只有出现至少一个 `created` 结果时，成功保存才会更新文件。

### 文本与数量规则：按入口区分

**解析清单文件（`ParseManifest`）**与**在 Go 代码里直接构造 `batchreg.Input` 提交（`Import`）**共用同一套登记文件格式和编号比对，但文本处理不同：

| 规则 | 清单文件经 `ParseManifest` | 直接构造 `Input` 经 `Import` |
| --- | --- | --- |
| 批次、产品、单位的首尾空白 | 去掉；去掉后不能为空，空或纯空白的字段拒绝该记录 | **不去**，按原样保存和逐字节比较 |
| 纯空白字符串（`" "`、`"\t"`） | 视为空白，拒绝 | 属于现有库接受的**非空**值，照常保存 |
| 空字符串 `""` | 拒绝 | 拒绝（库要求三个文本字段非空） |
| 大小写与内部字符（含内部空白） | 保留，`B1` 与 `b1` 是不同批次 | 同样保留，同样逐字节比较 |
| 非法 UTF-8 字节、孤立代理转义（`\ud800`） | 拒绝，绝不替换成 `�` | 拒绝，绝不替换成 `�` |
| 用户真实输入的替换字符 `�`（U+FFFD）、中文、emoji | 普通文本，接受 | 普通文本，接受 |
| 数量 | 必须是 **JSON 整数**文字，范围 1 到 9223372036854775807；字符串 `"120"`、小数 `1.0`/`1.5`、指数 `1e3`、零、负数、超范围一律拒绝 | `int64` 字段，有效值范围同为 1 到 9223372036854775807；没有 JSON 文字形状问题，零、负数和超范围值拒绝 |

直接构造记录的写法如下，注意首尾空白会成为值的一部分：

```go
results, err := batchreg.Import(reg, []batchreg.Input{
	{Batch: " B-200 ", Product: " P-8 ", Quantity: 30, Unit: " box "}, // 空白原样保留
	{Batch: " ", Product: "\t", Quantity: 1, Unit: "\n"},              // 纯空白也是非空值
})
```

因此从清单文件导入时，`" B-200 "` 只会与 `"B-200"` 相撞；直接构造 `Input` 时它们是两个不同批次。需要与命令行相同的去空白语义时，可在构造前调用公开的 `batchreg.NormalizeField`。

### 失败时的使用约定

**记录非法，或同编号内容冲突：整份导入被拒绝。** `Import` 先校验全部记录、在副本上合并，任何一条不合法或冲突都返回错误、`results` 为 `nil`、内存中的 `reg` 保持调用前内容。调用方不能展示部分成功，也不能继续调用 `Save`；即使非法记录排在最后，前面看似 `created` 的记录也不会留下。

- 非法记录（空文本、非法 UTF-8、数量越界等；`ParseManifest` 解析出的清单记录问题也是同一类型）返回 `*batchreg.ManifestRecordError`，用 `errors.As` 取出：`Position` 是清单中**从 1 开始**的记录位置；`Batch` 仅在批次编号自身合法且能唯一确定时给出，否则为空（绝不用替换后的 `�` 值指认批次）；具体字段写在错误信息里。例如：

  ```text
  manifest record 2 (batch "B2"): field "quantity" must be a JSON integer between 1 and 9223372036854775807
  ```

  清单文件整体为空、为空数组、不是 JSON 数组或 JSON 本身不合法时，`ParseManifest` 返回普通错误（无记录位置）。

- 同一批次编号已在登记文件中、或已在本次清单更早位置出现，但产品、数量、单位任一不同，返回 `*batchreg.ManifestConflictError`：`Position`（从 1 开始）、`Batch`、`Fields`（按 product、quantity、unit 顺序列出冲突字段）和冲突来源。`Source == "registry"` 表示与登记文件中的记录冲突；`Source == "manifest"` 时 `PrevPos` 给出该编号在清单中**首次出现**的从 1 开始位置：

  ```text
  manifest record 2 (batch "B1") conflicts with manifest record 1 on field(s): quantity; the whole manifest is rejected
  manifest record 1 (batch "B1") conflicts with the registered record on field(s): product, unit; the whole manifest is rejected
  ```

  ```go
  var conflict *batchreg.ManifestConflictError
  if errors.As(err, &conflict) {
      // conflict.Position, conflict.Batch, conflict.Fields
      // conflict.Source: "registry" 或 "manifest"
      // conflict.PrevPos: Source == "manifest" 时的更早记录位置
  }
  ```

**保存失败：必须报告登记文件路径与原因，先前返回的新增结果不算已完成登记。** `Save` 的错误形如 `cannot save registry "<登记文件路径>": <原因>`（原子保存：写入临时文件或最终替换失败时，已有文件字节与修改时间保持不变、仍可正常读取，原本不存在的路径仍不存在，临时文件会被清理）。此刻状态是分裂的：`Import` 已经成功，`results` 里标为 `created` 的新批次**已经进入内存中的 `reg`**（这是库的实际行为；重试前可检查 `len(reg.Batches)`），但磁盘登记文件还没有接受这些新增批次。因此：

- 向调用方报告登记文件**路径**和失败**原因**，不能把内存结果当作登记成功继续后续业务；
- 可以用同一个 `reg` 重试 `batchreg.Save(registryPath, reg)`（它仍持有完整、合法的新登记表），或放弃本次操作并重新 `batchreg.Load` 与磁盘对齐；
- 只有 `Save` 返回 `nil` 后，这批新增才算真正登记完成。

## 技术方向

dao, governance, voting, proposal, multisig, treasury-management, reputation-system

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
