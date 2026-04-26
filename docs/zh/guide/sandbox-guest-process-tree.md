# CubeSandbox Guest 内部进程树与启动阶段解析

本文梳理 CubeSandbox MicroVM Guest 内部的进程树、容器启动阶段，以及它与标准
OCI / `runc` 路径的关系。读这篇文档时要先区分两层视角：

- **Guest VM 视角**：`cube-agent` 是 VM 内的 `/sbin/init`，负责接收宿主机
  shim 的 ttrpc 请求，并在 VM 内创建、启动和管理容器进程。
- **容器视角**：业务镜像的 entrypoint / `CMD` 决定容器内看到的进程关系。
  Cube 官方基础镜像通常用 `cube-entrypoint.sh` 后台启动 `envd`，再把前台进程
  交给用户应用；如果没有用户 `CMD`，脚本会等待 `envd`。

## 1. 进程树全貌

CubeSandbox Guest 内部不依赖 `systemd`、`sshd`、`cron` 这类通用 Linux
服务。典型的运行时进程可以按“控制面”和“业务面”理解：

```text
Guest VM
└── PID 1: /sbin/init -> cube-agent
    ├── cube-agent init helper -> execve(...) -> container init process
    │   ├── /usr/bin/envd -port 49983              # 常见模板能力端点
    │   └── user foreground process / jupyter / app # 由镜像 entrypoint/CMD 决定
    └── cube-agent init helper -> execve(...) -> task exec process
        └── sh -c ... / python ... / debug command  # 仅底层 ExecProcess 路径
```

上图是**逻辑形态**，实际 `ps` 输出会受 PID namespace 和镜像 entrypoint 影响：

- 在 VM 根 PID namespace 里，`cube-agent` 固定承担 PID 1 / init 的职责。
- 在容器 PID namespace 里，容器 init process 会看到自己是 PID 1。
- `rustjail` 会先由 `cube-agent` 拉起一个 `cube-agent init` 子命令作为
  helper，完成 namespace、cgroup、rootfs、capability、seccomp 等设置后，
  最终 `execve` 成真正的业务进程，因此 helper 不是长期驻留服务。
- `envd` 是否是前台主进程取决于镜像 entrypoint。官方 `cube-entrypoint.sh`
  的常见模式是“后台 `envd` + 前台用户进程”；没有 `CMD` 时才会 `wait envd`。

> [!NOTE]
> **关于 `vsock-exporter`**
>
> `agent/vsock-exporter` 是被 `cube-agent` 引入的 Rust crate，用于 OpenTelemetry
> trace exporter；它不是 Guest 内一个长期独立的 `vsock-exporter` 进程。
> 相关初始化在 `agent/src/tracer.rs` 的 `setup_tracing()` 中完成。

## 2. 启动阶段

### 阶段一：VM Boot

**代表进程**：`cube-agent`，作为 Guest `/sbin/init` 运行。

MicroVM 启动后，Guest 内核把 `/sbin/init` 拉起为 PID 1。`cube-agent`
在 init 模式下会完成基础环境初始化：

- 挂载 `/proc`、`/sys`、`/dev` 和 cgroup 文件系统。
- 设置 `/dev/ptmx`、session、hostname、基础 PATH 等 init 环境。
- 启动 signal handler、uevent watcher、日志转发任务。
- 在配置的地址上启动 ttrpc server，供宿主机侧 `containerd-shim-cube-rs`
  通过 vsock 访问。

代码定位：

| 关注点 | 代码位置 |
| --- | --- |
| PID 1 / init 判断与基础挂载 | `agent/src/main.rs` |
| ttrpc server 启动 | `agent/src/rpc.rs` |
| 构建为 musl 静态二进制 | `agent/Makefile`、顶层 `Makefile` 的 `agent` target |

### 阶段二：CreateContainer

**代表动作**：宿主机 shim 发送 `CreateContainerRequest`，Guest 内创建容器对象和
rootfs / namespace / cgroup 准备状态。

这一步还不等同于业务进程已经开始执行。`cube-agent` 会：

- 把 shim 传入的 protobuf OCI spec 转为 `oci::Spec`。
- 处理 storage、rootfs、volume、设备和 guest hook。
- 调整 namespace 配置，例如是否共享 sandbox PID namespace。
- 创建 `LinuxContainer` 和 `Process` 对象，并通过 `rustjail` 启动容器 init
  helper。
- helper 在子进程中完成 PID namespace、mount namespace、rootfs pivot /
  move、用户与 capability 设置，最后等待启动信号。

源码主线：

```text
CubeShim/shim/src/container/mod.rs:create_container()
  -> agent::CreateContainerRequest
agent/src/rpc.rs:do_create_container()
  -> LinuxContainer::new(...)
  -> ctr.start(p)
agent/rustjail/src/container.rs:LinuxContainer::start()
  -> current_exe() "init"
  -> rustjail::container::init_child()
```

### 阶段三：StartContainer

**代表动作**：宿主机 shim 发送 `StartContainerRequest`，容器 init process 从
“准备好但被同步点挡住”进入 Running。

`rustjail` 为 init process 准备了 `exec.fifo` 同步点。`CreateContainer`
阶段会把子进程准备到可执行前，`StartContainer` 阶段调用 `ctr.exec()` 写入
FIFO，子进程继续执行并 `execve` 到 OCI spec 中定义的 entrypoint / args。

这也是业务镜像 entrypoint 开始生效的地方：

- 如果镜像使用官方 `cube-entrypoint.sh` 且带有 `CMD`，entrypoint 会先后台拉起
  `envd -port 49983`，再 `exec` 用户命令。
- 如果没有 `CMD`，entrypoint 会等待 `envd`，让模板能力端点保持常驻。
- 如果用户自带镜像完全自定义 entrypoint，只要按模板契约启动 `envd` 并通过探活，
  进程树可以和官方基础镜像不同。

### 阶段四：AppSnapshot Restore

命中 AppSnapshot 时，路径会和冷启动不同。Guest 内并不总是重新从零执行完整
entrypoint 初始化：

- shim 侧会根据快照恢复 VM 和容器状态。
- `cube-agent` 在 `CreateContainer` 中看到 AppSnapshot 相关 annotation 时，会走
  restore 分支，定位已恢复容器进程，并执行必要的 mount propagation 修复。
- shim 侧 `start_container()` 只在 cold start 场景向 agent 发送
  `StartContainerRequest`；非冷启动路径更多依赖恢复后的状态继续运行。

因此，文档和排障时要把“冷启动首次执行 entrypoint”和“快照恢复后继续运行”
分开看。

## 3. 用户命令执行的两条路径

### 路径 A：SDK / 模板能力路径

面向用户的 `Sandbox.commands.run()`、文件读写、初始化等模板能力，文档契约中是
访问容器内 `envd` 端点：

| 能力 | 容器内端点 |
| --- | --- |
| `Sandbox.commands.run()` | `POST :49983/process` |
| 文件读写 | `POST :49983/files` |
| 模板探活 | `GET :49983/health` |

这类请求的直接执行者通常是容器内的 `envd`。因此在进程树上，用户命令更可能表现为
`envd` 或用户应用/entrypoint 派生出的子进程，而不是每一次都经过
`containerd Task ExecProcess`。

### 路径 B：containerd Task ExecProcess 路径

底层 containerd exec、Cubelet 维护命令、调试命令等会走 shim 到 agent 的
`ExecProcess`：

```text
Cubelet / containerd task exec
  -> containerd-shim-cube-rs
  -> agent::ExecProcessRequest over ttrpc/vsock
  -> cube-agent do_exec_process()
  -> LinuxContainer::run()
  -> cube-agent init helper
  -> execve(target command)
```

在这个路径中，`cube-agent` 会通过 `rustjail` 创建一个新的 process，并把它加入目标
容器的 namespace / cgroup 语义中。实现上并不是调用外部 `runc exec`，而是由
`rustjail` 在 Guest 内完成 namespace join、cgroup apply、stdio 连接和最终
`execve`。

## 4. Guest 里有 `runc` 吗？

结论：**CubeSandbox 的 Guest 核心运行路径不依赖外置 `runc` 二进制。**

### Guest Runtime：无外置 `runc`，使用 `rustjail`

`cube-agent` 的 `Cargo.toml` 直接依赖本仓库的 `agent/rustjail` crate。
容器创建和 exec 的关键逻辑都在 `agent/rustjail/src/container.rs` 中：

- `LinuxContainer::start()` 拉起当前 `cube-agent` 二进制的 `init` 子命令。
- `init_child()` / `do_init_child()` 负责 namespace、rootfs、rlimit、用户、
  capability、seccomp 等 OCI 运行时动作。
- `join_namespaces()` 从父进程向子进程传递 spec、process、cgroup manager，并对
  目标 PID apply cgroup。
- 最终由子进程 `execve` 成目标业务命令。

这解释了为什么 Guest 里不会看到常驻 `runc`、`containerd` 或 Docker daemon。

### 宿主机 Shim：复用 containerd 接口，不代表调用 `runc`

宿主机侧的关键 runtime 是 `containerd-shim-cube-rs`。它实现 containerd Task
语义，把 `CreateContainer`、`StartContainer`、`ExecProcess` 等请求转发给 Guest
内的 `cube-agent`。仓库里会看到 containerd / OCI / runc option 相关类型，这是为了
兼容 containerd 的接口模型，不等于运行路径里执行了宿主机 `runc`。

### 模板构建：可能间接用到容器引擎

模板或基础镜像构建阶段可能会使用 Docker / containerd / 构建容器来产出 rootfs
或镜像 artifact。这属于离线构建流水线，不是用户沙箱启动后 Guest 内的运行时路径。
排查 Guest 进程树时不应把构建阶段的容器引擎进程混入运行时模型。

## 5. 排障速查

| 现象 | 优先检查 |
| --- | --- |
| Guest 内看不到 `systemd` | 预期行为，`cube-agent` 就是 PID 1 / init |
| Guest 内看不到 `runc` | 预期行为，Guest runtime 由静态编译进 `cube-agent` 的 `rustjail` 完成 |
| `Sandbox.commands.run()` 失败 | 先检查容器内 `envd` 是否监听 `:49983`，以及 `/health` 是否返回 204 |
| containerd exec / debug exec 失败 | 检查 shim 到 agent 的 `ExecProcess`、stdio、目标容器状态和 cgroup / namespace |
| AppSnapshot 恢复后进程没有重新初始化 | 预期行为，恢复路径会复用快照中的进程和内存状态 |

## 6. 源码索引

| 主题 | 文件 |
| --- | --- |
| Guest agent 角色说明 | `agent/README.md` |
| agent 启动、PID 1、ttrpc server | `agent/src/main.rs`、`agent/src/rpc.rs` |
| tracing / `vsock-exporter` crate 接入 | `agent/src/tracer.rs`、`agent/vsock-exporter/` |
| Guest OCI runtime 原语 | `agent/rustjail/src/container.rs` |
| shim 转发 Create / Start / Exec | `CubeShim/shim/src/container/mod.rs`、`CubeShim/shim/src/service/task_srv.rs` |
| Cubelet exec API | `Cubelet/services/cubebox/exec.go` |
| `envd` 模板契约 | `docs/zh/guide/tutorials/bring-your-own-image.md` |
