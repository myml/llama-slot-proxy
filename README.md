# llama-slot-proxy

[English](README.en.md) | **中文**

一个零依赖的 Go 代理，架在 [llama-server](https://github.com/ggml-org/llama.cpp) 前面，
让它的提示缓存能够**跨新会话复用**、并且**在服务重启后依然可用**。

实现方式是驱动 llama.cpp 自带的 `--slot-save-path` 与槽位 save/restore 接口。
代理本身不碰 KV 缓存，也不改写你的请求 —— 它只是在转发之前，请服务端恢复一份此前保存的状态。

```
客户端 ──► llama-slot-proxy :8080 ──► llama-server :8081
                    │
                    └── 与 --slot-save-path 共用同一个状态目录
```

---

## 要解决的问题

Agent 类客户端每一轮都会重发整段对话：很长的 system 提示词、完整的工具定义、以及全部历史。
llama-server 本来就能复用槽位里匹配的前缀，所以**延续**一段对话很便宜。贵的是其它情况：

| 情况 | llama-server 自己会怎么做 |
|---|---|
| **新会话**，system 提示词相同 | 把整段共享前缀重新预填充一遍 |
| **在两个会话之间切换** | 另一个会话的状态已被挤掉了 |
| **服务重启之后** | 槽位又空了 |

对于一份约 36 KB 的 agent system 提示词，这意味着每开一个新对话都要重算几千个 token；
而如果一段长会话的状态丢了，则要重算**整段历史**。

## 这个代理做了什么

缓存两种状态，都是 llama.cpp 原生的状态文件：

* **种子（seed）** —— 只预编码了 system 消息（加工具定义）之后的状态，别无其它。
  它是**所有**共用该 system 提示词的会话的严格前缀，因此对它们全都有效；
  并且以 `system + tools` 的哈希为键，所以 system 一变，自然就是一个新种子。
* **会话快照（session snapshot）** —— 某一段具体对话末尾的状态。
  只对该对话有效，因为同一对话的后续轮次是它的超集。

每个请求按「从便宜到昂贵」的顺序判断：

1. **是辅助请求吗？**（system 很短，例如生成标题）→ 纯透传，完全不碰缓存。
2. **这个会话的状态已经在槽位里了吗？** → 直接转发，不做任何 restore。
   后续交给 llama-server 自己的前缀匹配。
3. **存在本会话快照、且比种子更深吗？** → 恢复它。
4. **存在种子吗？** → 恢复种子。
5. **都还没有？** → 构建种子（一次预填充），然后转发。

## 实测数据

开发机：Ryzen AI MAX+ 395 / Radeon 8060S，61 GiB 统一内存；
Qwen3 系 27B 混合模型，4-bit 权重，`q8_0` KV，256K 上下文，单槽位。
以下是一段真实的约 104K token 的 agent 对话：

| | 预填充 token | 墙钟耗时 |
|---|---|---|
| **冷启动**，完全无缓存 | 103,928 | **608.6 s** |
| 同一对话的下一轮 | 19 | **4.8 s** |
| 同一对话，**重启 llama-server 之后** | 18 | **5.0 s** |

新会话共用该 system 提示词、上下文约 13.8K token 时：

| | 预填充 token | 墙钟耗时 |
|---|---|---|
| 无缓存 | 13,789 | 52.8 s |
| 恢复种子 | 5,604 | **22.9 s** |

恢复本身很便宜：一份 640 MB 的状态文件约 130 ms 恢复完。本机上状态文件大小服从

```
状态字节数 ≈ 149.63 MiB + 34.0 KiB × n_tokens
```

所以满 256K 上下文时每份状态文件约 8.65 GiB。请据此规划 `-max-bytes`。

## 前置条件

* 一个带 `--slot-save-path` 选项、以及 `/slots?action=save|restore` 接口的 llama.cpp 构建
  （上游早已具备）。
* **llama-server 必须写入代理所读取的同一个目录。** 代理只把一个**文件名**交给服务端，
  由服务端相对它自己的 `--slot-save-path` 解析。所以 `-cache-dir` 在两边必须是同一个路径 ——
  同一台主机、同一个挂载命名空间、同一个字符串。
* 假定单槽位（`-np 1`）。代理把缓存操作串行化在一把全局锁后面，因为恢复会覆盖槽位里现有的内容。
* 别无其它要求。不需要 cgo、不需要第三方模块、不需要构建服务。

## 快速开始

启动 llama-server 时给出 slot-save-path（推荐 tmpfs，原因见下）：

```bash
llama-server -m model.gguf -c 32768 -np 1 \
  --slot-save-path /dev/shm/llama-slot-proxy
```

在它前面启动代理，然后把客户端指向代理：

```bash
llama-slot-proxy \
  -listen 0.0.0.0:8080 \
  -upstream 127.0.0.1:8081 \
  -cache-dir /dev/shm/llama-slot-proxy \
  -persist-dir /var/lib/llama-slot-proxy
```

**建议先用 `-dry-run -v` 跑一次**：这时代理退化为纯透传，但仍会把每个上游响应的
`cache_n` / `prompt_n` 记进日志 —— 这是最快看出你的负载能省多少的办法。

### 构建

```bash
make build          # 或者：go build -o llama-slot-proxy .
```

需要 Go 1.21+。除标准库外没有任何依赖。

## 命令行参数

| 参数 | 默认值 | 含义 |
|---|---|---|
| `-listen` | `0.0.0.0:8080` | 监听地址。 |
| `-upstream` | `127.0.0.1:8081` | llama-server 地址。 |
| `-cache-dir` | `/dev/shm/llama-slot-proxy` | 工作目录。**必须等于** llama-server 的 `--slot-save-path`。 |
| `-persist-dir` | `""` | 持久目录：退出时收到一份副本，被淘汰的快照也移到这里。留空则全部留在 `-cache-dir`。 |
| `-max-bytes` | 16 GiB | `-cache-dir` 的容量上限。 |
| `-durable-bytes` | 32 GiB | `-persist-dir` 的容量上限。 |
| `-max-age` | 24h | 超过这个年龄的会话快照会被丢弃。 |
| `-max-seeds` | 4 | 保留多少个不同的种子。 |
| `-session-header` | `Session_id` | 携带会话 id 的请求头；找不到时回退尝试 `X-Client-Request-Id`。 |
| `-min-system-bytes` | 2000 | system 消息短于该字节数的请求完全不走缓存（用来过滤辅助请求）。0 = 全都缓存。 |
| `-require-stream` | false | 额外要求 `stream: true` 才缓存。 |
| `-save-every-tokens` | 8000 | 内容增长到这么多 token 后保存一次会话快照。0 = 只在槽位换手时保存。 |
| `-save-every-msgs` | 10 | 新增这么多条消息后保存一次。0 = 关闭。 |
| `-lock-wait` | 120s | 等待槽位锁的上限；超时则不加锁直接转发。 |
| `-log` | `""` | 日志文件；留空则写 stderr。 |
| `-dry-run` | false | 完全不碰缓存（纯透传）。 |
| `-v` | false | 详细日志。 |

## 保存策略是怎么定的

保存一份状态文件并不便宜（几百 MB），所以代理**不会**每个请求都保存：

* **策略 A —— 换手时保存。** 当另一个会话即将占走槽位时，先把当前会话存下来。
  这是它的状态还能被捕获的唯一时刻，也是稳态轮次**零磁盘写入**的原因。
* **策略 B —— 按内容增长保存。** 一直占着槽位的会话，在提示词增长到
  `-save-every-tokens` 个 token、或 `-save-every-msgs` 条消息时保存一次。
  增长按**内容**计量，绝不按时间，所以空闲期不产生任何开销，突发也不会漏掉。
* **退役。** 一个快照如果被恢复、却**没有**真正被复用，那它比没有快照更糟 ——
  因为这次恢复毁掉了 llama-server 本来可以用来回滚的检查点。
  连续两次未命中就把它丢掉。另外，如果一个种子已经覆盖了某个快照，也会把它丢掉。

启动时**不做任何预热**：只有请求真的需要时，才从 `-persist-dir` 把快照取回。

## tmpfs + 持久层分级

状态文件又大、又反复重写，所以直接写盘既慢、写放大又严重。开发机实测：

| 介质 | 写入 2 GB |
|---|---|
| tmpfs（`/dev/shm`） | 0.25 s（8.7 GB/s） |
| loop 设备上的 ext4 | 2.07 s（1.0 GB/s） |

所以工作目录放在 tmpfs，而 `-persist-dir` 是一份持久化的写回副本：
优雅退出时刷新一次，同时被工作集淘汰的快照会存放在那里。
只复制 mtime 更新过的文件，而且每次复制都先写 `*.tmp` 再 `rename`，
所以中途被打断也绝不会破坏一份快照。

收到 `SIGTERM`/`SIGINT` 时，先 drain（等在途请求结束，避免在生成中途捕获槽位），再落盘。
`SIGKILL` 和断电则会丢失尚未刷新的部分。

## 配置示例

```bash
llama-slot-proxy \
  -listen 0.0.0.0:8080 \
  -upstream 127.0.0.1:8081 \
  -cache-dir /dev/shm/llama-slot-proxy \
  -persist-dir /var/lib/llama-slot-proxy \
  -max-bytes $((5*1024*1024*1024)) \
  -durable-bytes $((24*1024*1024*1024)) \
  -save-every-tokens 8000 \
  -save-every-msgs 10 \
  -lock-wait 120s \
  -max-seeds 4 \
  -min-system-bytes 2000 \
  -v
```

一份最小的 systemd unit：

```ini
[Unit]
Description=llama-slot-proxy
After=network.target llama-server.service

[Service]
ExecStart=/usr/local/bin/llama-slot-proxy -cache-dir /dev/shm/llama-slot-proxy -persist-dir /var/lib/llama-slot-proxy -log /var/log/llama-slot-proxy.log
Restart=always
KillSignal=SIGTERM
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
```

## 注意事项

部署前请读一遍。

* **客户端必须逐字重发历史。** 这里的一切都建立在「已保存的序列是下一个提示词的严格前缀」
  之上。裁剪、摘要、或改写早期轮次的客户端，只会错过缓存。这是安全的（服务端会退回到
  正常的前缀匹配，代理也会退役反复未命中的快照），但确实意味着没有收益。
* **同一 session id 下发了更短的提示词时，会退回到种子。** 如果客户端复用同一个 session id
  开一个新对话，旧对话的快照比新提示词更深，会通不过前缀检验。代理会根据记录的消息条数
  识别出这一点，改用种子，所以新对话依然是热的。
* **状态文件会被解析文件头。** 代理读取前 12 字节（magic、version、token 数），
  以便在不恢复的前提下比较候选。如果 llama.cpp 改了那个磁盘格式，代理会把文件视作不存在 ——
  这是优雅降级而非崩溃，但缓存在更新解析逻辑之前会失效。
* **快照大小会被合理性检查**，依据是 `149.63 MiB + 34.0 KiB × n_tokens`：
  一次「空间不足」的保存会留下截断文件，而它的文件头仍然声称完整的 token 数。
  远低于估算值的文件会被忽略。这组常数来自某一个混合模型，只用作下限，不是精确校验。
* **`-min-system-bytes` 很重要。** 很多客户端会把同一个 session id 复用于一些小的旁路请求
  （生成标题之类）。它们很短的 system 提示词会构建一个没用的种子，并留下一个浅快照，
  下一轮污染真正的对话。默认值会把它们过滤掉。
* **单槽位。** 在 `-np 1` 下恢复会覆盖常驻状态，代理据此串行化。多槽位未被建模。
* **代理只关乎速度，不关乎正确性。** 即使所有缓存操作都失败，它也会原样转发请求。
  不存在任何一条「缓存失败会改变响应内容」的路径。

## 与 llama.cpp 上下文检查点的关系

代理与 llama.cpp 自己的上下文检查点（`-ctxcp`、`-cms`）解决的是**重叠但不同**的问题，
它们可以叠加使用：

* 代理负责**整块状态复用** —— 新会话，或槽位被抢占、服务重启后的会话，从一份已保存的前缀重新起步。
* 检查点负责**对话内部的回滚** —— 例如提示词尾部被替换时（重试、编辑、分支），
  只复用一段后缀，而不是整块。

两者都值得保留。注意：一次槽位恢复会替换掉提示词状态，所以当会话已经常驻时，
代理会刻意不做恢复，以免白白丢掉服务端进程内的检查点。

## 文件说明

```
main.go          请求处理、判定顺序、转发、配置
cache.go         种子、会话快照、保存策略、淘汰、槽位 API 客户端
persist.go       tmpfs 工作目录 + 持久副本、淘汰、按需取回
docs/DESIGN.md   规则背后的机制，附实测数据
```

## 许可

MIT —— 见 [LICENSE](LICENSE)。
