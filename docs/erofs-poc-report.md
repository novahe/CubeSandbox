# EROFS PoC 验证报告

## 结论

本次 PoC 验证通过：Cube VM 可以通过 pmem 同时挂载两个 EROFS 镜像。

- `/dev/pmem0` 作为 guest OS rootfs 启动，内核参数为 `rootfstype=erofs ro`
- `/dev/pmem1` 作为容器 rootfs EROFS 镜像，在 guest 内可挂载并读取文件
- 不依赖 snapshot，符合本次最小 PoC 验证目标

## 验证环境

| 项目 | 值 |
| --- | --- |
| 当前节点 | 已部署 Cube 一键部署环境 |
| Kernel | `/usr/local/services/cubetoolbox/cube-kernel-scf/vmlinux` |
| guest OS 原始镜像 | `/usr/local/services/cubetoolbox/cube-image/cube-guest-image-cpu.img` |
| guest OS EROFS 镜像 | `/usr/local/services/cubetoolbox/cube-image/cube-guest-image-cpu.img.erofs` |
| 容器镜像 | `ccr.ccs.tencentyun.com/ags-image/sandbox-code:latest` |
| 容器镜像 ID | `21b067114e86` |
| 容器 EROFS 镜像 | `/tmp/cube-erofs-poc/sandbox-code.erofs` |

## PoC 改动范围

| 模块 | 文件 | 说明 |
| --- | --- | --- |
| CLI 入口 | `CubeShim/cube-runtime/src/parser.rs` | 新增 `erofs-poc` 子命令 |
| CLI 分发 | `CubeShim/cube-runtime/src/main.rs` | 分发到 PoC 启动逻辑 |
| shim 模块导出 | `CubeShim/shim/src/lib.rs` | 导出 `poc` 模块 |
| PoC 启动器 | `CubeShim/shim/src/poc/cmd.rs` | 启动 VM，挂载两个 pmem EROFS 镜像 |
| VM 配置 | `CubeShim/shim/src/hypervisor/config.rs` | 根据 pmem `fs_type=erofs` 生成 `rootfstype=erofs ro` |
| VMM 配置结构 | `hypervisor/vmm/src/vm_config.rs` | `PmemConfig` 增加 `fs_type` 字段 |

## 启动命令

```bash
/data/nova/CubeSandbox/CubeShim/target/debug/cube-runtime erofs-poc \
  --kernel /usr/local/services/cubetoolbox/cube-kernel-scf/vmlinux \
  --pmem /usr/local/services/cubetoolbox/cube-image/cube-guest-image-cpu.img.erofs \
         /tmp/cube-erofs-poc/sandbox-code.erofs
```

## Guest 内验证

使用已有 debug console 登录：

```bash
/data/nova/CubeSandbox/CubeShim/target/debug/cube-runtime login <sandbox-id> --port 1026
```

验证结果：

| 验证项 | 结果 |
| --- | --- |
| guest OS rootfs 启动 | 成功 |
| 内核参数 | `root=/dev/pmem0 rootfstype=erofs ro` |
| guest OS rootfs 文件读取 | 成功读取 `/etc/os-release` |
| guest OS rootfs 内容 | `TencentOS Server 4.4` |
| 第二块 pmem | `/dev/pmem1` 可见 |
| 容器 EROFS 挂载 | 成功挂载到 `/dev/shm/poc-mnt1` |
| 容器 rootfs 文件读取 | 成功读取 `/dev/shm/poc-mnt1/etc/os-release` |
| 容器 rootfs 内容 | `Debian GNU/Linux 13 (trixie)` |

说明：guest OS rootfs 是只读挂载，`/tmp` 和 `/root` 不能创建挂载点，本次使用 `/dev/shm/poc-mnt1` 作为临时挂载目录。

## 恢复与重建

如果 `cube-guest-image-cpu.img.erofs` 被误删，可以直接用现存的 ext4 guest 镜像重建：

```bash
mkdir -p /tmp/cube-guest-mnt
mount -o loop,ro /usr/local/services/cubetoolbox/cube-image/cube-guest-image-cpu.img /tmp/cube-guest-mnt
mkfs.erofs -zlz4 /usr/local/services/cubetoolbox/cube-image/cube-guest-image-cpu.img.erofs /tmp/cube-guest-mnt
umount /tmp/cube-guest-mnt
```

重建结果：

| 项目 | 结果 |
| --- | --- |
| 输出文件 | `/usr/local/services/cubetoolbox/cube-image/cube-guest-image-cpu.img.erofs` |
| 实际大小 | `335M` |
| 文件类型 | `EROFS filesystem` |

## 镜像体积对比

| 对象 | 原始大小 | EROFS 大小 | 减少量 | 降幅 | 结论 |
| --- | ---: | ---: | ---: | ---: | --- |
| guest OS rootfs | `769M` | `335M` | `434M` | `56.4%` | 收益明确，值得做 |
| 容器 rootfs 解包目录 | `4.7G` | `2.6G` | `2.1G` | `44.7%` | 收益明确，值得做 |
| 容器镜像报告值 | `5.13GB` | `2.6G` | 约 `2.5G` | 约 `49%` | 作为参考口径 |

## 技术可行性

| 项目 | 结论 |
| --- | --- |
| 双 EROFS pmem 挂载 | 可行 |
| guest OS EROFS 启动 | 可行 |
| 容器 rootfs EROFS 挂载 | 可行 |
| snapshot 依赖 | 本次 PoC 未使用 snapshot |
| 对主链路影响 | 当前改动集中在独立 PoC 入口，风险较低 |
| 主要限制 | pmem 镜像大小需要按 VMM 要求对齐，本次容器 EROFS 已补齐到 2MiB 边界 |

## 工作量预估

| 阶段 | 工作内容 | 预估 |
| --- | --- | ---: |
| PoC 验证 | 独立 CLI 启动 VM，挂两个 EROFS，guest 内手工验证 | `0.5-1 人日` |
| 工具化 | 固化镜像构建、对齐、启动、login 验证命令 | `1-2 人日` |
| 接入运行链路 | 接入容器 rootfs EROFS 生成和挂载，处理生命周期 | `3-5 人日` |
| 端到端验证 | 覆盖容器启动、回收、异常清理、兼容性测试 | `3-5 人日` |
| 生产化 | 参数化、日志、错误处理、CI、回滚策略 | `5+ 人日` |

## 建议

短期建议继续推进 EROFS 方向，优先级如下：

| 优先级 | 内容 | 原因 |
| --- | --- | --- |
| P0 | 容器 rootfs EROFS 化 | 体积从 `4.7G` 降到 `2.6G`，收益最大 |
| P0 | guest OS rootfs EROFS 化 | 体积从 `769M` 降到 `335M`，收益明确 |
| P1 | 固化镜像构建脚本 | 避免手工流程引入偏差 |
| P1 | 固化 pmem 对齐处理 | 避免 `PmemSizeNotAligned` 启动失败 |
| P2 | 自动化 guest 验证 | 便于 CI 或回归验证 |

总体判断：技术路径成立，体积收益明确，建议进入工具化和端到端验证阶段。
