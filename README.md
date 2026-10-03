# CidVault

这是一个面向内容寻址存储的内容寻址的去中心化存储与检索平台。长期目标是提供内容标识与分块、块存储与垃圾回收、引脚与保留策略、网关寻址、存储证明、加密封装和去重传输，把内容寻址存储沉淀为可复用平台。

仓库采用 Go，当前冻结基线只提供进程健康检查。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/cidvault
```

服务默认监听 `127.0.0.1:8080`。可通过 `CIDVAULT_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 内容寻址对象库

单进程内存对象库，仅在进程存活期间可用，不承诺重启保留；不包含远端路由和访问控制。上传默认不自动建立引脚，引脚状态同样不承诺重启保留。

- `POST /v1/objects`：以 `application/octet-stream` 上传原文。正文按 1048576 字节切分（末块可不足，空正文无数据块）。首次接收返回 `201`，重复对象返回 `200`；响应 JSON 为 `{"cid","size","chunkSize","chunks","created"}`，其中 `chunks` 按重建顺序排列，`created` 表示本次是否新增。
- `GET /v1/objects/{cid}`：以 `application/octet-stream` 返回逐字节一致的原文。
- `GET /v1/objects/{cid}/manifest`：返回 `{"cid","size","chunkSize","chunks"}`，客户端可据此校验块与根标识并按序还原内容。

标识与清单编码：

- 块标识为 `sha256:<64位小写十六进制>`，摘要取块原始字节。
- 根标识取同一格式，对确定性清单字节求摘要：首行 `size:<对象总长度十进制>\n`，随后每行一个块标识（含 `\n`），按重建顺序排列。例如空对象的清单字节为 `size:0\n`。

限制与失败语义：单个对象最多 67108864 字节，超限返回 `413` 与 `payload_too_large` 且不留部分对象；媒体类型错误返回 `415` 与 `unsupported_media_type`；路径标识格式错误返回 `400` 与 `invalid_cid`；格式有效但对象不存在返回 `404` 与 `object_not_found`；方法不允许返回 `405` 并带 `Allow` 头。错误正文统一为 `application/json` 的 `{"error":{"code":"..."}}`。

## 块读取与存储统计

块按内容去重存储：同一块在对象内重复或被多个对象共享时只保留一份，统计中也只计一次。

- `GET /v1/blocks/{cid}`：以 `application/octet-stream` 返回块的原始字节，`Content-Length` 为实际块长。只要仍有当前对象引用该块即可读取；标识格式错误返回 `400` 与 `invalid_cid`，格式有效但未被当前对象引用返回 `404` 与 `block_not_found`。
- `GET /v1/storage/stats`：返回 `{"objects","logicalBytes","blocks","storedBytes"}` 的同一时点一致快照。`objects` 为当前对象数（空对象计入），`logicalBytes` 为这些对象正文长度之和，`blocks` 为被当前对象引用的唯一块数，`storedBytes` 为这些唯一块原始字节之和。重复上传同一对象不改变统计。

正式回收删除对象后，不再被引用的块立即不可读并从统计中移除；仍被其他对象引用的共享块继续可读且只计一次。预览回收（`dryRun=true`）不改变对象、块与统计。

## 引脚与垃圾回收

对象引脚控制回收保留，内容标识规则不受影响。

- `PUT /v1/pins/{cid}`：为已有对象建立或更新引脚。媒体类型须为 `application/json`，正文只可含可选的 `expiresAt`；省略或传 `null` 表示永久保留，非空值须为严格晚于受理时刻的 RFC3339 时间。当前无有效引脚时建立返回 `201`，否则更新返回 `200`；响应为 `{"cid","expiresAt"}`。到期时间格式错误或不在未来返回 `422` 与 `invalid_expiration`；畸形 JSON 或未知字段返回 `400` 与 `invalid_request`；媒体类型不符返回 `415` 与 `unsupported_media_type`；对象不存在返回 `404` 与 `object_not_found`。
- `GET /v1/pins`：返回 `{"pins":[{"cid","expiresAt"},...]}`，仅含有效引脚，永久项的 `expiresAt` 为 `null`，按 cid 字典序排列；到期引脚不返回。
- `DELETE /v1/pins/{cid}`：删除成功返回 `204`；引脚不存在或已到期返回 `404` 与 `pin_not_found`。
- `POST /v1/gc`：回收受理时没有有效引脚的对象（到期引脚等同未引脚，永久或未到期引脚保留）。带 `dryRun=true` 时只预览不删除，`dryRun` 取其他值返回 `400` 与 `invalid_request`。响应为 `{"dryRun","objects","bytes","cids"}`，`objects` 为候选或已删除对象数，`bytes` 为其正文长度之和，`cids` 按字典序排列。被回收对象的正文与清单读取返回 `404` 与 `object_not_found`；重新上传相同内容仍得到原 CID。引脚与回收互为原子：加引脚先成功则同次回收不得删除，回收先删除则加引脚返回 `object_not_found`。

## 单对象离线包

离线包用于把单个对象连同其全部块跨实例搬运，媒体类型为 `application/vnd.cidvault.bundle+json`。包体为 JSON：`version` 固定为 1；`root` 含现有清单的 `cid`、`size`、`chunkSize`、`chunks`；`blocks` 含清单引用的唯一块，每项为 `{"cid","data"}`，`data` 为 RFC 4648 标准 Base64，块按 cid 字典序排列，空对象为空数组。

- `GET /v1/bundles/{cid}`：导出对象离线包。路径标识格式错误返回 `400` 与 `invalid_cid`，对象不存在返回 `404` 与 `object_not_found`。
- `POST /v1/bundles`：导入同一格式的离线包。服务先完整验证再原子写入并复用已有块：`size` 为不超过上限的非负整数，`chunkSize` 等于固定值；块标识须与解码数据的 SHA-256 一致；`chunks` 保留顺序与重复项，`blocks` 不得缺块、重复或含未引用块；非空对象按现有分块规则还原，空对象不得含块，重建长度等于 `size`，重算根标识等于 `root.cid`。响应结构与普通上传相同：首次插入返回 `201` 且 `created` 为 `true`，已有相同对象返回 `200` 与 `false`；导入不自动建立引脚；并发导入同一对象只有一个首次插入。

失败语义：媒体类型缺失或不符返回 `415` 与 `unsupported_media_type`；畸形 JSON、尾随内容、缺失、未知或重复字段、字段类型错误、非法 Base64 或包内标识格式错误返回 `400` 与 `invalid_bundle`；`version` 或 `chunkSize` 不支持返回 `422` 与 `unsupported_bundle`；声明或重建大小超限返回 `413` 与 `payload_too_large`；摘要、块集合、分块长度、对象长度或根标识校验失败返回 `422` 与 `integrity_check_failed`。任何失败都不改变对象、块、引脚或统计。

## 跨实例增量离线包

增量离线包只搬运接收方尚未持有的块，媒体类型为 `application/vnd.cidvault.delta-bundle+json`。包体结构与完整离线包相同：`version` 固定为 1，`root` 沿用现有清单，`blocks` 为清单引用但接收方未持有的唯一块，按块标识字典序排列，全部命中时为空数组。

- `POST /v1/delta-bundles/{cid}`：导出差量包。请求媒体类型须为 `application/json`，正文为 `{"have":[...]}`，列出接收方已有的块标识；清单未引用的条目忽略。成功返回 `200` 与增量包。正文结构或字段非法、标识重复或格式错误返回 `400` 与 `invalid_request`；媒体类型不符返回 `415` 与 `unsupported_media_type`；路径标识格式错误返回 `400` 与 `invalid_cid`；对象不存在返回 `404` 与 `object_not_found`。
- `POST /v1/delta-bundles`：导入增量包。服务结合本地仍被对象引用的块与包内块重建对象，完整校验后原子写入并复用已有块；所需本地块不存在返回 `422` 与 `missing_block`。响应与普通上传相同：首次创建返回 `201` 且 `created` 为 `true`，已有对象返回 `200` 与 `false`；导入不建立引脚，块继续去重，并发导入同一根标识仅一个请求首次创建。包保留清单块顺序与重复引用，空对象允许空 `blocks`；导入后的原文、清单、块读取与统计与直接上传一致。

失败语义与完整包对应：非单个严格 JSON、字段缺失、重复、未知或类型错误、非法 Base64 或包内标识格式错误返回 `400` 与 `invalid_delta_bundle`；`version` 或 `chunkSize` 不支持返回 `422` 与 `unsupported_delta_bundle`；声明或重建大小超限返回 `413` 与 `payload_too_large`；块摘要、块集合、分块长度、对象长度或根标识不一致返回 `422` 与 `integrity_check_failed`。任何失败都不改变对象、块、引脚或统计。两个入口不支持的方法返回 `405`、对应 `Allow` 头与 `method_not_allowed`。

## 验证

```bash
go test ./...
```

当前基线刻意不包含内容标识与分块、块存储与存储证明的实现，以便后续任务从已冻结事实出发独立设计并验证这些能力。
