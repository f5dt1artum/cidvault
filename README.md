# CidVault

这是一个面向内容寻址存储的内容寻址的去中心化存储与检索平台。长期目标是提供内容标识与分块、块存储与垃圾回收、引脚与保留策略、网关寻址、存储证明、加密封装和去重传输，把内容寻址存储沉淀为可复用平台。

仓库采用 Go，当前冻结基线只提供进程健康检查。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/cidvault
```

服务默认监听 `127.0.0.1:8080`。可通过 `CIDVAULT_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 内容寻址对象库

单进程内存对象库，仅在进程存活期间可用，不承诺重启保留；不包含远端路由和访问控制。对象不可变，只能通过显式垃圾回收删除。

- `POST /v1/objects`：以 `application/octet-stream` 上传原文。正文按 1048576 字节切分（末块可不足，空正文无数据块）。首次接收返回 `201`，重复对象返回 `200`；响应 JSON 为 `{"cid","size","chunkSize","chunks","created"}`，其中 `chunks` 按重建顺序排列，`created` 表示本次是否新增。
- `GET /v1/objects/{cid}`：以 `application/octet-stream` 返回逐字节一致的原文。
- `GET /v1/objects/{cid}/manifest`：返回 `{"cid","size","chunkSize","chunks"}`，客户端可据此校验块与根标识并按序还原内容。

标识与清单编码：

- 块标识为 `sha256:<64位小写十六进制>`，摘要取块原始字节。
- 根标识取同一格式，对确定性清单字节求摘要：首行 `size:<对象总长度十进制>\n`，随后每行一个块标识（含 `\n`），按重建顺序排列。例如空对象的清单字节为 `size:0\n`。

限制与失败语义：单个对象最多 67108864 字节，超限返回 `413` 与 `payload_too_large` 且不留部分对象；媒体类型错误返回 `415` 与 `unsupported_media_type`；路径标识格式错误返回 `400` 与 `invalid_cid`；格式有效但对象不存在返回 `404` 与 `object_not_found`；方法不允许返回 `405` 并带 `Allow` 头。错误正文统一为 `application/json` 的 `{"error":{"code":"..."}}`。

## 引脚、到期保留与垃圾回收

上传后默认不引脚；引脚状态只保存在进程内存中，不承诺重启保留。引脚与对象标识无关，不改变 CID 规则。

- `PUT /v1/pins/{cid}`：为已有对象建立或更新引脚。媒体类型须为 `application/json`，正文只可含可选的 `expiresAt`（如 `{"expiresAt":"2026-10-03T12:00:00Z"}`）。省略或传 `null` 表示永久保留；非空值须为严格晚于受理时刻的 RFC3339 时间。当前无有效引脚时返回 `201`，更新有效引脚返回 `200`，响应为 `{"cid","expiresAt"}`，永久项 `expiresAt` 为 `null`。对象不存在返回 `404` 与 `object_not_found`；CID 格式错误返回 `400` 与 `invalid_cid`。
  - 媒体类型不符：`415` 与 `unsupported_media_type`。
  - 畸形 JSON、未知字段、非对象正文：`400` 与 `invalid_request`。
  - `expiresAt` 格式错误或时间不在未来：`422` 与 `invalid_expiration`。
- `GET /v1/pins`：返回 `{"pins":[{"cid","expiresAt"}]}`，仅含当前有效引脚，永久项 `expiresAt` 为 `null`，按 `cid` 字典序排列；已到期引脚不返回，空列表为 `"pins":[]`。
- `DELETE /v1/pins/{cid}`：删除有效引脚返回 `204`；引脚不存在或已到期返回 `404` 与 `pin_not_found`；CID 格式错误返回 `400` 与 `invalid_cid`。
- `POST /v1/gc`：执行显式垃圾回收。带 `dryRun=true` 只预览；其他 `dryRun` 取值返回 `400` 与 `invalid_request`。响应为 `{"dryRun","objects","bytes","cids"}`：`objects` 为候选（预览）或已删除对象数，`bytes` 为其正文长度之和，`cids` 按字典序排列（空为 `[]`）。回收只处理受理时没有有效引脚的对象；到期引脚等同未引脚，永久或未到期引脚保留。删除后对象正文与清单读取均返回既有 `404` 与 `object_not_found`；重新上传相同内容仍得到原 CID。
- 并发语义：引脚与回收在同一把锁内裁决，加引脚成功则同次回收不会删除该对象；回收先删除则随后的加引脚返回 `object_not_found`。删除对读者原子化，读取不会观察到部分正文或残缺清单。
- 新接口对不支持的方法返回 `405`、正确的 `Allow` 头（`/v1/pins/{cid}` 为 `DELETE, PUT`，`/v1/pins` 为 `GET`，`/v1/gc` 为 `POST`）与既有 JSON 错误结构。

## 验证

```bash
go test ./...
```

当前基线刻意不包含内容标识与分块、块存储与存储证明的实现，以便后续任务从已冻结事实出发独立设计并验证这些能力。
