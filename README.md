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
- `POST /v1/gc`：回收受理时既无有效引脚也不属于任何有效递归引脚记录成员的对象（到期记录等同不存在，永久或未到期记录保留）。带 `dryRun=true` 时只预览不删除，`dryRun` 取其他值返回 `400` 与 `invalid_request`。响应为 `{"dryRun","objects","bytes","cids"}`，`objects` 为候选或已删除对象数，`bytes` 为其正文长度之和，`cids` 按字典序排列。被回收对象的正文与清单读取返回 `404` 与 `object_not_found`；重新上传相同内容仍得到原 CID。引脚与回收互为原子：加引脚先成功则同次回收不得删除，回收先删除则加引脚返回 `object_not_found`。

## 递归引脚

递归引脚把整棵目录子树作为一条记录保留：根对象须为已存在的合法目录，服务按目录格式递归遍历，`directory` 项继续下探，`file` 项为叶子，同一 CID 只计一次。遍历、存在性检查与提交在同一时点完成，任何失败都不留下记录；递归引脚先提交即保护全部成员，回收先删除所需对象则建立失败。

- `PUT /v1/recursive-pins/{cid}`：建立或替换该根 CID 的递归引脚记录。媒体类型须为 `application/json`，正文为单个严格 JSON 对象，只可含可选的 `expiresAt`；省略或传 `null` 表示永久保留，非空值须为严格晚于受理时刻的 RFC3339 时间。首次建立或替换已到期记录返回 `201`，更新有效记录返回 `200`。响应为 `{"cid","expiresAt","objects","bytes","members"}`：`members` 为提交时快照，含根及全部后代，按 CID 字典序排列；`objects` 为唯一成员对象数；`bytes` 为成员正文长度之和。路径 CID 非法返回 `400` 与 `invalid_cid`；媒体类型不符返回 `415` 与 `unsupported_media_type`；正文不是单个严格 JSON、含未知或重复字段返回 `400` 与 `invalid_request`；到期值非法返回 `422` 与 `invalid_expiration`；根对象不存在返回 `404` 与 `object_not_found`；根或下探到的目录对象不是合法目录返回 `422` 与 `invalid_directory`；被引用对象缺失返回 `424` 与 `recursive_pin_target_missing`；遍历深度超过 64（根为第 0 层）或唯一成员超过 10000 返回 `422` 与 `recursive_pin_too_large`。
- `GET /v1/recursive-pins/{cid}`：返回有效记录及创建时的成员快照，结构同上；记录不存在或已到期返回 `404` 与 `recursive_pin_not_found`。
- `DELETE /v1/recursive-pins/{cid}`：删除有效记录返回 `204`；记录不存在或已到期返回 `404` 与 `recursive_pin_not_found`。删除只移除保留记录，不立即删除成员对象，也不写审计事件。

有效递归引脚与直接引脚共同决定 `POST /v1/gc` 的保留集合，预览规则相同；删除或到期一条记录不释放仍受其他递归记录或直接引脚保护的对象。递归成员不出现在 `GET /v1/pins`；递归引脚的建立、更新与删除均不写审计事件。该入口仅允许 `GET, PUT, DELETE`，其他方法返回 `405`、对应 `Allow` 头与 `method_not_allowed`。

## 单对象离线包

离线包用于把单个对象连同其全部块跨实例搬运，媒体类型为 `application/vnd.cidvault.bundle+json`。包体为 JSON：`version` 固定为 1；`root` 含现有清单的 `cid`、`size`、`chunkSize`、`chunks`；`blocks` 含清单引用的唯一块，每项为 `{"cid","data"}`，`data` 为 RFC 4648 标准 Base64，块按 cid 字典序排列，空对象为空数组。

- `GET /v1/bundles/{cid}`：导出对象离线包。路径标识格式错误返回 `400` 与 `invalid_cid`，对象不存在返回 `404` 与 `object_not_found`。
- `POST /v1/bundles`：导入同一格式的离线包。服务先完整验证再原子写入并复用已有块：`size` 为不超过上限的非负整数，`chunkSize` 等于固定值；块标识须与解码数据的 SHA-256 一致；`chunks` 保留顺序与重复项，`blocks` 不得缺块、重复或含未引用块；非空对象按现有分块规则还原，空对象不得含块，重建长度等于 `size`，重算根标识等于 `root.cid`。响应结构与普通上传相同：首次插入返回 `201` 且 `created` 为 `true`，已有相同对象返回 `200` 与 `false`；导入不自动建立引脚；并发导入同一对象只有一个首次插入。

失败语义：媒体类型缺失或不符返回 `415` 与 `unsupported_media_type`；畸形 JSON、尾随内容、缺失、未知或重复字段、字段类型错误、非法 Base64 或包内标识格式错误返回 `400` 与 `invalid_bundle`；`version` 或 `chunkSize` 不支持返回 `422` 与 `unsupported_bundle`；声明或重建大小超限返回 `413` 与 `payload_too_large`；摘要、块集合、分块长度、对象长度或根标识校验失败返回 `422` 与 `integrity_check_failed`。任何失败都不改变对象、块、引脚或统计。

## 跨实例增量离线包

增量离线包在两个实例间只搬运接收方尚缺的块，媒体类型为 `application/vnd.cidvault.delta-bundle+json`，包体结构与完整离线包一致：`version` 固定为 1，`root` 沿用现有清单，`blocks` 为本次随包携带的块子集，仍为标准 Base64 并按 cid 字典序排列。

- `POST /v1/delta-bundles/{cid}`：发送方导出。请求媒体类型须为 `application/json`，正文为 `{"have":[...]}`，列出接收方已有的块标识；无关标识忽略，标识重复或格式错误、正文结构或字段非法返回 `400` 与 `invalid_request`，媒体类型不符返回 `415` 与 `unsupported_media_type`，路径标识非法返回 `400` 与 `invalid_cid`，对象不存在返回 `404` 与 `object_not_found`。成功返回 `200` 与增量包媒体类型，`blocks` 只含清单引用但未列入 `have` 的唯一块，按 cid 字典序排列；全部命中时为空数组，空对象同样为空。
- `POST /v1/delta-bundles`：接收方导入。服务把本地仍被对象引用的块与包内块合并，按清单顺序与重复引用重建对象，完整校验后原子写入；清单引用但两处都没有的块返回 `422` 与 `missing_block`。非单个严格 JSON、字段缺失、重复、未知或类型错误、非法 Base64、包内标识格式错误返回 `400` 与 `invalid_delta_bundle`；`version` 或 `chunkSize` 不受支持返回 `422` 与 `unsupported_delta_bundle`；声明或重建大小超限返回 `413` 与 `payload_too_large`；块摘要、块集合（包内块重复或未被引用）、分块长度、对象长度或根 CID 不一致返回 `422` 与 `integrity_check_failed`；媒体类型缺失或不符返回 `415` 与 `unsupported_media_type`。任何失败都不改变对象、块、引脚或统计。

成功导入沿用普通上传响应：首次创建返回 `201` 且 `created` 为 `true`，已有对象返回 `200` 与 `false`；导入不建立引脚，块继续跨对象去重，并发导入同一根 CID 仅一个请求首次创建。导入后的原文、清单、块读取与统计与直接上传相同；空对象允许空 `blocks`（此时无需任何块）。两个新入口对不支持的方法返回 `405`、对应 `Allow` 头与 `method_not_allowed`。

## 提供者目录

提供者目录让节点按 CID 发布访问位置。目录不要求本机存有对象：可为任意格式合法的 CID 发布；发布不建立引脚、不参与回收或存储统计，记录仅在进程内存中、不承诺重启保留。

- `PUT /v1/providers/{cid}/{providerId}`：受理时原子建立或替换该键的记录。媒体类型须为 `application/json`，正文严格为 `{"addresses":[...],"expiresAt":"..."}`，字段缺一不可且无未知或重复字段。`providerId` 为 1 至 64 个小写字母、数字或连字符且首尾为字母或数字；`addresses` 含 1 至 16 个互不重复的绝对 `http`/`https` URL，禁止用户信息（userinfo）、片段（fragment）与空主机，并须按字符串严格升序排列；`expiresAt` 须为严格晚于受理时刻且不超过其后 24 小时的 RFC3339 时间。首次发布或替换已到期记录返回 `201`，更新有效记录返回 `200`；响应为包含 `providerId`、`addresses` 和 `expiresAt` 的完整记录。
- `GET /v1/providers/{cid}`：返回 `{"cid":"...","providers":[...]}`，仅含受理时有效的记录，按 `providerId` 字典序排列；该 CID 无记录时返回 `200` 与空数组。
- `DELETE /v1/providers/{cid}/{providerId}`：删除有效记录返回 `204`；记录不存在或已到期返回 `404` 与 `provider_not_found`。

并发语义：所有变更与到期判断在同一把锁下完成，查询返回同一时点的快照（记录内容为拷贝），同一键并发首次发布恰好一个返回 `201`，到期项不会再次出现。

失败语义：路径 CID 非法返回 `400` 与 `invalid_cid`；`providerId` 非法返回 `400` 与 `invalid_provider`；PUT 媒体类型不符返回 `415` 与 `unsupported_media_type`；非单个严格 JSON、字段缺失、未知或重复字段及类型错误（含 `null` 元素）返回 `400` 与 `invalid_request`；地址不合规（数量越界、重复、未排序、非绝对 http(s)、含用户信息/片段/空主机等）返回 `422` 与 `invalid_address`；`expiresAt` 格式错误、已到期或超过受理时刻后 24 小时上限返回 `422` 与 `invalid_expiration`。`GET /v1/providers/{cid}` 只允许 GET（`Allow: GET`），单提供者入口允许 `PUT, DELETE`；不支持的方法返回 `405`、正确的 `Allow` 头与 `method_not_allowed`。

## 主动检索

主动检索按 CID 从提供者目录登记的地址取回本地缺失的对象，入口为 `POST /v1/retrievals/{cid}`，不接收请求正文。路径 CID 非法返回 `400` 与 `invalid_cid`；不支持的方法返回 `405`、`Allow: POST` 与 `method_not_allowed`。

- 对象在受理时已存在：不访问网络，返回 `200`，响应与上传相同的 `cid`、`size`、`chunkSize`、`chunks`，另含 `created=false`、`providerId=null`、`address=null`。
- 本地缺失：服务取得受理时点仍有效的提供者快照，按 `providerId` 字典序及各记录原有地址顺序逐一尝试。每个地址就是完整 URL，以 GET 请求，不拼接路径、不携带调用方认证信息、不跟随重定向。网络失败、超时、非 200 状态、媒体类型非 `application/octet-stream`、正文超过对象大小上限，或按现有分块与根标识规则重算后 CID 不匹配，都只使当前地址失败并继续下一个。
- 没有有效提供者返回 `404` 与 `no_provider`；所有地址均失败返回 `502` 与 `retrieval_failed`，且不留对象、块、引脚或统计变化。
- 首个合法原文结束尝试，按普通上传语义原子写入并复用已有块：新建返回 `201` 与 `created=true`，并发请求已写入同一对象时返回 `200` 与 `created=false`，同一 CID 的并发检索至多一个 `201`。远端成功时响应另含实际采用的 `providerId` 与 `address`。导入不自动建立引脚；之后对象、清单、块与统计入口的表现与直接上传一致。检索期间使用受理时快照，记录随后到期或删除不改变本次尝试顺序。

## 主动多副本复制

主动多副本复制把本地已存对象的一次完整离线包并行推送到多个远端导入入口，入口为 `POST /v1/replications/{cid}`。请求媒体类型须为 `application/json`，正文严格为 `{"targets":[...],"required":N}`：`targets` 为 1 至 16 个互不重复、按字符串严格升序排列的绝对 `http`/`https` URL，路径须恰为 `/v1/bundles`，不得含用户信息、查询或片段；`required` 为 1 至目标数的整数。封装对象不参与复制。

校验通过后，服务从受理时本地快照生成一次完整离线包（与 `GET /v1/bundles/{cid}` 相同的格式），以完整离线包媒体类型并行发送同一包至全部目标：不转发调用方认证信息、不跟随重定向、单目标五秒超时。远端返回 `200` 或 `201`，且响应 JSON 中 `cid` 等于源 CID、`created` 为布尔值时该目标成功。

成功响应为 `200`，含 `cid`、`required`、`succeeded`、`failed`、`metRequirement` 与 `results`；两个计数依次为成功、失败数。`results` 按 `targets` 顺序排列，每项含 `target`、`ok`、`result`：成功项的 `result` 依远端 `created` 为 `created` 或 `existing`；网络错误与超时为 `unreachable`；重定向、其他状态码或不合约响应为 `rejected`。成功数达到 `required` 时 `metRequirement` 为 `true`。单个目标的失败不取消其他请求，已成功导入的远端副本不回滚。

失败语义：路径 CID 非法返回 `400` 与 `invalid_cid`；本地对象不存在返回 `404` 与 `object_not_found`；媒体类型缺失或不符返回 `415` 与 `unsupported_media_type`；非单个严格 JSON 或字段缺失、未知、重复、类型错误返回 `400` 与 `invalid_request`；`targets` 或 `required` 取值违规返回 `422` 与 `invalid_replication_request`；这些失败均不访问远端。该入口仅允许 `POST`，其他方法返回 `405`、`Allow: POST` 与 `method_not_allowed`。

复制不改变本地业务状态（对象、块、引脚、统计均不变），也不写本地审计事件；各远端沿用既有包导入、去重与审计语义，成功导入在其本地记录 `bundle.import`。

## 可检索性证明

可检索性证明是只读入口，调用方凭随机挑战验证本机确实持有某对象的块数据，入口为 `POST /v1/retrievability-proofs/{cid}`。请求媒体类型须为 `application/json`，正文严格为 `{"nonce":"...","samples":N}`：`nonce` 为规范 RFC 4648 标准 Base64（解码后 16 至 64 字节，解码再编码须逐字节还原输入），`samples` 为 1 至 16 的整数。

成功返回 `200`，响应 JSON 含 `cid`、规范化 `nonce`、`requestedSamples` 与 `entries`；每项含清单位置 `index`、块 `cid` 及原始块的标准 Base64 `data`。条目数取 `samples` 与清单位置数的较小值，空对象返回空数组；重复块的各个位置分别参与抽样。

抽样位置可独立复算：为清单每个位置计算 SHA-256，输入按顺序拼接 ASCII 字节 `cidvault-retrievability-v1`、换行、根 cid、换行、规范化 nonce、换行、十进制 index、换行；按摘要字节无符号字典序排列，摘要相同按 index 升序，选取所需数量，`entries` 最终按 index 升序返回。调用方可用公开清单核对选位，以 `data` 重算块 cid 并确认位置归属。一次请求读取同一时点的对象、清单和块；并发回收时只能完整成功或返回对象不存在，不得出现缺项、错配或部分响应。

失败语义：路径 cid 非法返回 `400` 与 `invalid_cid`；对象不存在返回 `404` 与 `object_not_found`；媒体类型缺失或不符返回 `415` 与 `unsupported_media_type`；非单个严格 JSON、字段缺失、未知、重复或类型错误返回 `400` 与 `invalid_request`；nonce 非规范 Base64 或解码长度越界返回 `422` 与 `invalid_nonce`；samples 越界返回 `422` 与 `invalid_sample_count`；不支持的方法返回 `405`、`Allow: POST` 与 `method_not_allowed`。此入口不建立引脚、不写审计事件，也不改变对象、块、提供者或存储统计。

## 只读网关

只读网关在现有对象库上按路径逐层解析已存目录对象，最终取得文件或子目录，并沿用内容标识、引脚与提供者语义（引脚与回收照常作用于被引用对象；网关本身不发起远端检索、不建立引脚、不改变统计）。

目录对象仍以 `application/octet-stream` 经 `POST /v1/objects` 按现有分块与根标识规则保存；目录的媒体类型仅在网关返回时使用。目录正文为严格 UTF-8 JSON，顶层仅含 `version` 和 `entries`；`version` 固定为 1，`entries` 最多 4096 项。每项仅含互不重复的 `name`、`type`、`cid`：`name` 为 1 至 255 个 Unicode 码点的单一路径段，不得为 `.` 或 `..`，不得含斜杠、反斜杠、NUL 或其他控制字符，并按 Unicode 码点严格递增排列；`type` 仅为 `file` 或 `directory`；`cid` 沿用现有 `sha256:<64位小写十六进制>` 格式。

- `GET /v1/gateway/{cid}` 与 `GET /v1/gateway/{cid}/{path...}`：根 CID 须为已存目录对象。`path` 按 `/` 分段，每段仅百分号解码一次，以解码结果精确匹配，不做 Unicode 归一化。中间项须为 `directory`；最终项为 `file` 时返回该对象原始字节，`Content-Type` 为 `application/octet-stream`；最终项为 `directory` 或省略 `path` 时返回目录对象原文（不重新序列化），`Content-Type` 为 `application/vnd.cidvault.directory+json`。成功响应均给出准确的 `Content-Length`。

路径校验：每段百分号解码恰好一次（`+` 为字面量）；一次请求至多解析 64 段（不含根 CID）。整个解析以受理时对象库的同一一致快照完成（单次读锁内走完整条路径），并发回收不会使一个请求看到不同时点的对象；网关只读本地已存对象，条目录用的本地对象缺失时直接失败，不访问提供者地址。

失败语义：根 CID 非法返回 `400` 与 `invalid_cid`；路径存在非法转义、解码非 UTF-8、空段，或解码结果含斜杠/反斜杠、等于 `.`/`..`、超过 64 段返回 `400` 与 `invalid_path`；根对象缺失返回 `404` 与 `object_not_found`；某层条目不存在返回 `404` 与 `path_not_found`；试图穿过 `file` 继续向下返回 `409` 与 `not_directory`；按目录读取的对象不符合上述目录契约返回 `422` 与 `invalid_directory`；条目引用的本地对象缺失返回 `424` 与 `gateway_target_missing`。错误正文沿用现有 `application/json` 的 `{"error":{"code":"..."}}` 结构；其余方法返回 `405`、`Allow: GET, HEAD` 与 `method_not_allowed`。网关读取不增加审计事件，既有入口行为保持不变。

## 条件读取、断点读取与 HEAD

`GET /v1/objects/{cid}`、`GET /v1/blocks/{cid}` 与全部 `GET /v1/gateway/{cid}/{path...}` 支持条件缓存、单范围断点读取与 HEAD，既有读取语义不变：普通 GET 仍返回逐字节一致的全文、原 `Content-Type` 与准确 `Content-Length`。

- 强 ETag：对象与网关取（网关最终解析到的）对象 CID，块取块 CID，均以双引号包围完整 CID；成功响应另带 `Accept-Ranges: bytes`。
- `If-None-Match` 按 HTTP 弱比较接受 `*`、单个标签与逗号分隔标签，任一匹配时先于 `Range` 返回无正文的 `304`；无法解析为这些形式返回 `400` 与 `invalid_request`。其他条件头不新增语义。
- `Range` 仅接受一个 `bytes` 范围：闭区间、指定起点至末尾或后缀长度。可满足时返回 `206`、实际闭区间与完整长度的 `Content-Range` 及所选字节数的 `Content-Length`，覆盖全文亦然；超过末尾的终点或后缀截到正文边界。单位非 `bytes`、语法非法、多范围、起点越界、终点早于起点、零长度后缀或空正文的任意范围，统一返回 `416`、`range_not_satisfiable` 与 `Content-Range: bytes */<完整长度>`。
- `HEAD` 沿用对应 GET 的 CID、路径、目录及目标存在性校验，忽略 `Range`，返回完整响应应有的状态、`ETag`、`Accept-Ranges`、`Content-Type` 与 `Content-Length`，但任何状态都不写正文；`If-None-Match` 匹配仍返回 `304`。
- 三个入口仅允许 `GET, HEAD`，其他方法返回 `405`、`Allow: GET, HEAD` 与 `method_not_allowed`。错误优先级为既有路径和资源错误、`If-None-Match`、`Range`，不掩盖 `invalid_cid`、`object_not_found`、`block_not_found` 或网关解析错误。这些读取不建立引脚、不触发远端检索或审计，也不改变对象、块、元数据、引用和存储统计。

## 审计事件流

进程内审计事件流记录变更与检索入口的成功结果，仅在进程存活期间可用，不承诺重启保留。审计不参与引脚、回收、存储统计或内容寻址；失败请求与审计读取本身不记录。事件在业务状态提交后可见，并发时事件顺序与提交顺序一致。仅保留最近 10000 条，淘汰最旧记录后 `seq` 继续递增且不复用。

- `GET /v1/audit/events`：返回 `{"events":[...],"nextAfter":N,"hasMore":B}`。`events` 来自同一时点快照并按 `seq` 升序；`after` 可选，仅返回序号更大的事件；`limit` 为 1 到 1000 的十进制整数，默认 100。`nextAfter` 取本页最后一条序号，空页取 `after` 或 0；`hasMore` 表示该快照仍有后续事件。尚无事件时返回 `200` 与空数组。

事件字段：`seq` 为严格递增序号；`at` 为 UTC RFC3339Nano 时间；`action` 取 `object.upload`、`bundle.import`、`delta_bundle.import`、`pin.put`、`pin.delete`、`gc.preview`、`gc.collect`、`provider.put`、`provider.delete`、`retrieval.local` 或 `retrieval.fetch`；`result` 取 `created`、`existing`、`updated`、`deleted`、`collected`、`previewed`、`local`、`retrieved_created` 或 `retrieved_existing`；`cid`、`objects`、`bytes`、`providerId` 不适用时为 `null`。对象类操作的 `bytes` 为正文长度，回收操作取响应总字节数；`objects` 仅回收操作适用，`providerId` 仅提供者变更与远端取回适用。

失败语义：非零 `after` 早于当前最早保留事件的前一序号时返回 `410` 与 `audit_cursor_expired`；重复参数、未知查询参数、非法整数或越界 `limit` 返回 `400` 与 `invalid_request`；不支持的方法返回 `405`、`Allow: GET` 与 `method_not_allowed`。

## 对象元数据与修订历史

已存对象可携带进程内元数据，不改变 CID 与正文。每次成功写入生成连续递增的修订号（从 1 开始），内容未变也保留为新修订；历史修订可单独读取。

- `PUT /v1/objects/{cid}/metadata`：媒体类型须为 `application/json`，正文严格为 `{"expectedRevision":N,"contentType":T,"labels":{...}}`。首次写入 `N` 为 0，之后须等于当前修订号；相同 `expectedRevision` 的并发更新仅一个成功，其余返回 `409` 与 `revision_conflict`。首次写入返回 `201`，后续返回 `200`，响应含 `cid`、`revision`、`contentType`、`labels` 与 UTC RFC3339Nano 格式的 `updatedAt`，`labels` 按键字典序编码。
- `GET /v1/objects/{cid}/metadata`：无查询参数返回当前修订；带正十进制 `revision` 参数读取指定历史修订，响应结构与 PUT 相同。

取值约束：`contentType` 可为 `null`，或为 1 至 255 个 Unicode 码点且不含控制字符的字符串；`labels` 最多 64 项，键须匹配 `[a-z0-9][a-z0-9._-]{0,63}`，值不超过 256 个 Unicode 码点且不含控制字符。

失败语义：非单个严格 JSON，字段缺失、未知、重复、类型错误或 `expectedRevision` 非非负整数返回 `400` 与 `invalid_request`；取值越界或违规返回 `422` 与 `invalid_metadata`；媒体类型不符返回 `415` 与 `unsupported_media_type`；路径 CID 非法返回 `400` 与 `invalid_cid`；对象不存在返回 404 与 `object_not_found`；尚无元数据返回 `404` 与 `metadata_not_found`；指定修订不存在返回 `404` 与 `metadata_revision_not_found`；GET 的未知或重复参数、非法 `revision` 返回 `400` 与 `invalid_request`；其余方法返回 `405`、`Allow: GET, PUT` 与 `method_not_allowed`。任何失败都不产生修订。

元数据不建立引脚，不计入存储统计或离线包，也不新增审计事件。正式回收对象时其元数据历史随对象原子删除，预览回收（`dryRun=true`）不改变历史；重新上传相同 CID 不恢复旧历史，元数据从修订 1 重新开始。

## 命名引用与修订历史

命名引用把一个名称指向已存对象，不改变对象 CID 与正文。名称以百分号解码一次后的结果为准，须匹配 `[a-z0-9][a-z0-9._-]{0,127}`。每次成功写入生成连续递增的修订号（从 1 开始），即使指向的 CID 未变也新增修订；历史修订可单独读取。

- `PUT /v1/refs/{name}`：媒体类型须为 `application/json`，正文严格为 `{"cid":"...","expectedRevision":N}`，无未知、重复或多余字段。首次发布 `N` 为 0，之后须等于当前修订号；目标对象须在受理时存在。相同 `expectedRevision` 的并发更新仅一个成功，其余返回 `409` 与 `revision_conflict`。首次写入返回 `201`，后续返回 `200`；响应含 `name`、`revision`、`cid` 与 UTC RFC3339Nano 格式的 `updatedAt`。
- `GET /v1/refs/{name}`：无查询参数返回当前修订；带正十进制 `revision` 参数读取指定历史修订，响应结构与 PUT 相同。
- `GET /v1/refs/{name}/object`：返回所选修订指向对象的原始字节，给出准确 `Content-Length`，并带 `X-CidVault-CID` 与 `X-CidVault-Ref-Revision` 响应头。名称与历史修订的解析和对象读取在同一一致快照内完成（单次读锁），并发回收不会使一次请求看到不同时点的对象。

失败语义：名称非法返回 `400` 与 `invalid_ref`；PUT 媒体类型不符返回 `415` 与 `unsupported_media_type`；非单个严格 JSON，结构、字段或类型错误，或 `expectedRevision` 不是非负整数返回 `400` 与 `invalid_request`；CID 非法返回 `400` 与 `invalid_cid`；CID 合法但目标对象不存在返回 `404` 与 `object_not_found`；修订冲突返回 `409` 与 `revision_conflict`；名称不存在返回 `404` 与 `ref_not_found`；历史修订不存在返回 `404` 与 `ref_revision_not_found`；GET 的未知或重复参数、非法 `revision` 返回 `400` 与 `invalid_request`。`/v1/refs/{name}` 允许 `GET, PUT`，`/v1/refs/{name}/object` 仅允许 `GET`；其余方法返回 `405`、正确 `Allow` 头与 `method_not_allowed`。

引用不充当引脚、不阻止垃圾回收、不计入存储统计，也不进入离线包或产生审计事件。目标对象被回收后，引用当前与历史信息仍可读取；对象入口对该引用的每个修订固定返回 `424` 与 `reference_target_missing`。重新上传相同内容恢复对象后，引用对象入口随之恢复。引用仅在进程存活期间保留，不承诺重启保留。

## 加密封装对象

加密封装对象与公开明文对象库完全隔离：调用方在 `X-CidVault-Key` 头提供密钥，服务只保存每次随机加密后的封装，内存中不保留密钥或原文。封装对象仅在进程内存中保留、不承诺重启保留，不参与既有对象生命周期（分块、引脚、垃圾回收）、提供者目录、检索、离线包、审计事件或存储统计；既有入口行为保持不变。

- `POST /v1/sealed-objects`：请求媒体类型须为 `application/octet-stream`，正文沿用 67108864 字节上限。`X-CidVault-Key` 须为 RFC 4648 标准 Base64（含规范填充）编码的 32 字节密钥。服务使用 AES-256-GCM，每次请求新生成 12 字节 nonce，关联数据为 ASCII 字节 `cidvault-sealed-v1\nsize:<原文长度十进制>\n`。CID 沿用 `sha256:<64位小写十六进制>` 格式，对关联数据、nonce 与含 GCM 认证标签的密文依次拼接求 SHA-256。成功返回 `201` 与 `{"cid","size","algorithm"}`，`algorithm` 固定为 `AES-256-GCM`。
- `GET /v1/sealed-objects/{cid}`：携带正确密钥时以 `application/octet-stream` 返回逐字节一致的原文，给出准确 `Content-Length`，并带 `Cache-Control: no-store`。
- `GET /v1/sealed-objects/{cid}/envelope`：无需密钥。返回媒体类型 `application/vnd.cidvault.sealed+json`，正文严格为 `{"version":1,"algorithm","cid","size","nonce","ciphertext"}`；`nonce` 与 `ciphertext`（含认证标签）均为标准 Base64。调用方可据此重算并核对 CID，并在链下独立解密，服务不因此暴露原文。
- `DELETE /v1/sealed-objects/{cid}`：要求正确密钥，认证通过后删除，成功返回 `204`。删除之后再读取或删除均返回 `404` 与 `sealed_object_not_found`。

并发语义：封装的插入与删除在独立锁下原子完成，读取要么得到完整原文要么得到不存在，不会出现部分内容；同一 CID 的并发删除恰好一个成功，其余返回 `sealed_object_not_found`。

失败语义：缺少密钥返回 `401` 与 `key_required`，并带 `WWW-Authenticate: CidVaultKey`；密钥存在但不是规范标准 Base64 或解码后不是 32 字节返回 `400` 与 `invalid_key`；密钥可解析但认证失败返回 `403` 与 `access_denied`。POST 媒体类型缺失或错误返回 `415` 与 `unsupported_media_type`，正文超限返回 `413` 与 `payload_too_large`；路径 CID 格式非法返回 `400` 与 `invalid_cid`；格式合法但封装不存在（含已删除）返回 `404` 与 `sealed_object_not_found`。集合入口仅允许 `POST`（`Allow: POST`），对象入口允许 `GET, DELETE`，封装入口仅允许 `GET`（`Allow: GET`）；其余方法返回 `405`、正确 `Allow` 头与 `method_not_allowed`。错误正文统一为 `application/json` 的 `{"error":{"code":"..."}}`。任何失败都不留下封装状态。

## 验证

```bash
go test ./...
```

当前基线刻意不包含内容标识与分块、块存储与存储证明的实现，以便后续任务从已冻结事实出发独立设计并验证这些能力。
