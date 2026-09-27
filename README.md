# 菌株序列标签认证服务

纯后端服务，使用 Go 1.23、标准库 `net/http` 与 PostgreSQL。服务维护批次草稿；封存时把整批输入冻结，计算每条记录相对批次内其他记录的最短唯一连续片段，并生成重启后仍可读取和独立复算的认证报告。

## 快速启动

```bash
docker compose up --build
```

- API: `http://localhost:8080`
- PostgreSQL: `localhost:5432`
- 默认账号/密码/数据库：`seqlabels/seqlabels/seqlabels`

API 启动时自动执行幂等数据库迁移。

本地直接运行：

```bash
docker compose up -d db
DATABASE_URL='postgres://seqlabels:seqlabels@localhost:5432/seqlabels?sslmode=disable' \
  go run ./cmd/server
```

## 数据约束

- 记录 ID：非空 ASCII，长度 1–128，字符仅限 `A-Z`、`a-z`、`0-9`、`_`、`-`。
- 序列：非空，仅包含小写字母 `a-z`。
- 单条序列最长 200,000 字节；同一批次全部序列总长不超过 500,000 字节。
- 草稿可反复新增、替换、删除记录；封存后批次和报告均不可修改。
- 任何非法写入都会在事务中被拒绝，原草稿保持不变。

## 接口

所有请求和响应均使用 JSON。错误响应格式稳定：

```json
{ "error": { "code": "BATCH_SEALED", "message": "batch is sealed and cannot be modified" } }
```

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/healthz` | 健康检查，会探测数据库 |
| `POST` | `/batches` | 创建空批次草稿，请求体为 `{}` 或空 |
| `GET` | `/batches/{batchId}` | 读取批次草稿及当前记录 |
| `PUT` | `/batches/{batchId}/records/{recordId}` | 新增或替换一条记录 |
| `DELETE` | `/batches/{batchId}/records/{recordId}` | 删除一条记录；删除不存在的 ID 也返回 204 |
| `POST` | `/batches/{batchId}/seal` | 计算、写入不可变报告并封存批次 |
| `GET` | `/batches/{batchId}/report` | 读取封存报告 |

### 维护记录

```bash
BATCH_ID='...'

curl -sS -X PUT "http://localhost:8080/batches/$BATCH_ID/records/a" \
  -H 'Content-Type: application/json' \
  -d '{"sequence":"ababa"}' -i

curl -sS -X PUT "http://localhost:8080/batches/$BATCH_ID/records/b" \
  -H 'Content-Type: application/json' \
  -d '{"sequence":"babab"}' -i

curl -sS -X PUT "http://localhost:8080/batches/$BATCH_ID/records/c" \
  -H 'Content-Type: application/json' \
  -d '{"sequence":"zzaba"}' -i
```

成功写入/删除返回 `204 No Content`。

### 封存与读取报告

```bash
curl -sS -X POST "http://localhost:8080/batches/$BATCH_ID/seal"
curl -sS "http://localhost:8080/batches/$BATCH_ID/report"
```

报告包含：

```json
{
  "id": "report-uuid",
  "batchId": "batch-uuid",
  "algorithm": "shortest-unique-substring/v1",
  "inputHash": "sha256-of-canonical-frozen-input",
  "sealedAt": "2026-01-01T00:00:00Z",
  "records": [
    { "id": "a", "sequence": "ababa" },
    { "id": "b", "sequence": "babab" },
    { "id": "c", "sequence": "zzaba" }
  ],
  "labels": {
    "a": { "start": 0, "end": 5, "substring": "ababa" },
    "b": { "start": 0, "end": 5, "substring": "babab" },
    "c": { "start": 0, "end": 1, "substring": "z" }
  }
}
```

不存在唯一片段的记录在 `labels` 中显式为 `null`。例如两条完全相同的序列都会返回 `null`。

## 索引口径

`start` 与 `end` 都按序列的字节位置解释，即：

- `start` 是 **零基起点**；
- 区间为 **左闭右开**，长度等于 `end - start`；
- 片段恒等于 `sequence[start:end]`。

因为输入只允许单字节 ASCII 小写字母，字节位置与字符位置一致。选择答案时：

1. 先取最短长度；
2. 同长度取零基 `start` 最小者；
3. 片段允许在**同一记录内部**重复出现；
4. 只要该连续片段出现在任何其他记录中，就不是该记录的唯一标签；
5. 没有任何唯一连续片段时返回 `null`。

示例：`ababa`、`babab`、`zzaba` 中，前两条记录的较短片段都能在另一条中找到，只能取整串；第三条从起点 0 的 `z` 不在其他记录中出现。

## 唯一性算法和性能口径

封存时构造一条由批次全文组成的广义后缀数组：每条记录后添加不可能出现在 `a-z` 输入中的零字节分隔符。对每个后缀，只需找到后缀数组左、右最近且属于其他记录的后缀，并用 Kasai LCP 数组与区间最小值结构求最长公共前缀。候选唯一片段长度为“最大公共前缀 + 1”。

该算法：

- 按批次总长 `N` 为 `O(N log N)` 时间、`O(N)` 内存；
- 不会对每个候选片段再扫描其他记录全文；
- 不会使用空串、整串占位或预填结果；
- 在 500,000 字节、高重叠最大批次上按 5 秒内完成设计；
- 报告保存完整冻结输入、算法版本和 SHA-256 输入哈希，可用 `labels.Compute` 独立复算并逐项核对。

## 稳定错误代码

| HTTP 状态 | code | 场景 |
|---:|---|---|
| 400 | `INVALID_JSON` | JSON 语法错误、未知字段、多个 JSON 值 |
| 400 | `INVALID_RECORD_ID` | ID 字符或长度非法 |
| 400 | `INVALID_SEQUENCE` | 序列缺失、为空或含非 `a-z` 字符 |
| 404 | `BATCH_NOT_FOUND` | 批次不存在 |
| 404 | `REPORT_NOT_FOUND` | 批次尚未封存或报告不存在 |
| 405 | `METHOD_NOT_ALLOWED` | 路径存在但 HTTP 方法不支持 |
| 409 | `BATCH_SEALED` | 封存后尝试修改 |
| 422 | `SEQUENCE_TOO_LONG` | 单条超过 200,000 |
| 422 | `BATCH_TOO_LARGE` | 批次总长超过 500,000 |
| 500 | `INTERNAL_ERROR` | 未暴露细节的服务端错误 |
| 503 | `DATABASE_UNAVAILABLE` | 健康检查无法连接数据库 |

## 测试

算法测试包含题目示例、完全相同序列、确定性随机小输入和朴素枚举对拍。朴素参考实现按起点、长度枚举候选，并直接在其他全文中搜索，仅用于测试。

先启动真实 PostgreSQL：

```bash
docker compose up -d db
```

运行全部测试：

```bash
DATABASE_URL='postgres://seqlabels:seqlabels@localhost:5432/seqlabels?sslmode=disable' \
  go test ./...
```

高重叠最大批次性能基准：

```bash
go test ./internal/labels -bench=ComputeHighOverlapMaximumBatch -benchmem
```

集成测试为每个测试创建独立 PostgreSQL schema，并在结束时删除；测试连接的是真实存储，而不是内存模拟。
