// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Result};
use clap::Args;
use std::fs;
use std::path::PathBuf;
use std::sync::mpsc::channel;
use std::thread;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use crate::common::utils::Utils;
use crate::common::utils::VM_PATH;
use crate::hypervisor::config::VmConfig;
use crate::sandbox::pmem::Pmem;
use cube_hypervisor::{self, ApiRequest, NotifyEvent, VmmInstance};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;
use tokio::time::timeout;

#[derive(Args, Debug)]
pub struct ErofsPocArgs {
    /// Guest kernel path
    #[arg(long = "kernel", value_name = "kernel path", required = true)]
    pub kernel: String,

    /// EROFS pmem image paths. The first entry becomes /dev/pmem0 rootfs.
    /// The second entry becomes /dev/pmem1 so you can inspect the container image in guest.
    #[arg(long = "pmem", value_name = "pmem path", required = true, num_args = 2..)]
    pub pmem: Vec<String>,

    /// CPU count
    #[arg(long = "cpu", default_value_t = 2)]
    pub cpu: u32,

    /// Memory size in MiB
    #[arg(long = "memory", default_value_t = 2048)]
    pub memory: u64,

    /// Sandbox ID. Defaults to a generated id.
    #[arg(long = "id")]
    pub id: Option<String>,

    /// Debug console port inside the guest.
    #[arg(long = "console-port", default_value_t = 1026)]
    pub console_port: u32,

    /// Guest rootfs files to cat for verification.
    #[arg(long = "root-file")]
    pub root_file: Vec<String>,

    /// Mounted image files to cat for verification.
    #[arg(long = "image-file")]
    pub image_file: Vec<String>,
}

pub async fn execute(args: ErofsPocArgs) -> Result<()> {
    if args.pmem.len() < 2 {
        return Err(anyhow!("need at least two --pmem paths"));
    }

    let id = args.id.clone().unwrap_or_else(gen_poc_id);
    let vm_dir = PathBuf::from(VM_PATH).join(&id);
    fs::create_dir_all(&vm_dir)
        .map_err(|e| anyhow!("create vm dir {} failed: {}", vm_dir.display(), e))?;

    cube_hypervisor::set_runtime_seccomp_rules(vec![
        (libc::SYS_mkdir, vec![]),
        (libc::SYS_getsockopt, vec![]),
        (libc::SYS_setsockopt, vec![]),
        (libc::SYS_faccessat2, vec![]),
    ]);

    let mut vmm_config = cube_hypervisor::vmm_config::VmmConfig {
        sandbox_id: id.clone(),
        ..Default::default()
    };
    let (sender, receiver) = channel::<NotifyEvent>();
    vmm_config.event_notifier =
        Some(cube_hypervisor::vmm_config::EventNotifyConfig { notifier: sender });

    let mut vm_config = VmConfig::default();
    vm_config.pmems = Some(Vec::new());
    vm_config
        .set_kernel(args.kernel.clone())
        .set_vcpus(args.cpu)
        .set_memory(args.memory, true)
        .add_pmems(&build_pmems(&args.pmem))
        .add_vsock(id.clone());

    let ch =
        VmmInstance::new(vmm_config).map_err(|e| anyhow!("create VMM instance failed: {}", e))?;
    let b_vm_config = Box::new(vm_config.to_vm_config());

    ch.send_request(ApiRequest::VmCreate(b_vm_config))
        .map_err(|e| anyhow!("Create vm failed: {}", e))?
        .map_err(|e| anyhow!("Create vm failed: {}", e))?;
    ch.send_request(ApiRequest::VmBoot)
        .map_err(|e| anyhow!("Boot vm failed: {}", e))?
        .map_err(|e| anyhow!("Boot vm failed: {}", e))?;

    let ev = receiver
        .recv_timeout(Duration::from_secs(10))
        .map_err(|e| anyhow!("wait vm ready failed: {}", e))?;
    if ev != NotifyEvent::SysStart && ev != NotifyEvent::VsockServerReady {
        return Err(anyhow!("unexpected vm event: {:?}", ev));
    }

    println!("vm started: {}", id);
    println!("guest root pmem: {}", args.pmem[0]);
    println!("guest extra pmem: {}", args.pmem[1]);
    println!("vsock path: {}", Utils::vsock_path(&id).display());
    run_guest_verification(&id, &args).await?;
    println!("press Ctrl-C to stop");

    let _ = tokio::signal::ctrl_c().await;
    thread::sleep(Duration::from_millis(200));
    Ok(())
}

fn build_pmems(paths: &[String]) -> Vec<Pmem> {
    paths
        .iter()
        .enumerate()
        .map(|(idx, file)| Pmem {
            file: file.clone(),
            discard_writes: true,
            fs_type: "erofs".to_string(),
            source_dir: String::new(),
            id: if idx == 0 {
                "root".to_string()
            } else {
                format!("extra-{}", idx)
            },
            size: None,
            placeholder: false,
        })
        .collect()
}

fn gen_poc_id() -> String {
    let since_epoch = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    format!("erofs-poc-{}-{}", std::process::id(), since_epoch)
}

async fn run_guest_verification(id: &str, args: &ErofsPocArgs) -> Result<()> {
    let vsock_path = Utils::vsock_path(id);
    let conn = hybrid_vsock_dialer(&vsock_path, args.console_port, Duration::from_secs(10)).await?;
    let (mut reader, mut writer) = conn.into_split();

    let root_files = if args.root_file.is_empty() {
        vec![
            "/etc/os-release".to_string(),
            "/etc/issue".to_string(),
            "/etc/alpine-release".to_string(),
        ]
    } else {
        args.root_file.clone()
    };
    let image_files = if args.image_file.is_empty() {
        vec![
            "/tmp/poc-mnt1/etc/os-release".to_string(),
            "/tmp/poc-mnt1/etc/issue".to_string(),
            "/tmp/poc-mnt1/bin/sh".to_string(),
        ]
    } else {
        args.image_file.clone()
    };

    let script = r#"set -eux; echo __POC_BEGIN__; echo __ROOTFS_START__; cat /proc/cmdline; for f in __ROOT_FILES__; do if [ -f "$f" ]; then echo "__ROOT_FILE:$f__"; cat "$f"; break; fi; done; ls -l /dev/pmem0 /dev/pmem1; echo __ROOTFS_DONE__; mkdir -p /tmp/poc-mnt1; mount -t erofs /dev/pmem1 /tmp/poc-mnt1; echo __IMAGEFS_START__; for f in __IMAGE_FILES__; do if [ -e "$f" ]; then echo "__IMAGE_FILE:$f__"; cat "$f"; fi; done; find /tmp/poc-mnt1 -maxdepth 2 -type f | head -n 20; echo __IMAGEFS_DONE__; echo __POC_DONE__"#;
    let script = script
        .replace("__ROOT_FILES__", &join_shell_words(&root_files))
        .replace("__IMAGE_FILES__", &join_shell_words(&image_files));

    writer
        .write_all(script.as_bytes())
        .await
        .map_err(|e| anyhow!("send guest verification command failed: {}", e))?;
    writer
        .write_all(b"\n")
        .await
        .map_err(|e| anyhow!("send guest verification newline failed: {}", e))?;
    writer.flush()
        .await
        .map_err(|e| anyhow!("flush guest verification command failed: {}", e))?;

    let mut out = String::new();
    let mut buf = [0u8; 4096];
    loop {
        match timeout(Duration::from_secs(2), reader.read(&mut buf)).await {
            Ok(Ok(0)) => break,
            Ok(Ok(n)) => {
                let chunk = String::from_utf8_lossy(&buf[..n]);
                print!("{}", chunk);
                out.push_str(&chunk);
                if out.contains("__POC_DONE__") {
                    break;
                }
            }
            Ok(Err(e)) => return Err(anyhow!("read guest output failed: {}", e)),
            Err(_) => break,
        }
    }

    if !out.contains("__POC_DONE__") {
        return Err(anyhow!("guest verification did not complete"));
    }

    Ok(())
}

fn join_shell_words(items: &[String]) -> String {
    items
        .iter()
        .map(|item| shell_quote(item))
        .collect::<Vec<_>>()
        .join(" ")
}

fn shell_quote(s: &str) -> String {
    if s.chars().all(|c| c.is_ascii_alphanumeric() || "/._-:".contains(c)) {
        return s.to_string();
    }
    format!("'{}'", s.replace('\'', r"'\''"))
}

async fn hybrid_vsock_dialer(
    vsock_path: &PathBuf,
    port: u32,
    timeout_duration: Duration,
) -> Result<UnixStream> {
    if port == 0 {
        return Err(anyhow!("Port not specified"));
    }

    let mut conn = timeout(timeout_duration, UnixStream::connect(vsock_path))
        .await
        .map_err(|e| anyhow!("connect timeout: {}", e))??;

    conn.write_all(format!("CONNECT {}\n", port).as_bytes())
        .await
        .map_err(|e| anyhow!("send connect cmd failed: {}", e))?;

    let mut reader = tokio::io::BufReader::new(conn);
    let mut response = String::new();
    reader
        .read_line(&mut response)
        .await
        .map_err(|e| anyhow!("read connect response failed: {}", e))?;

    if response.contains("OK") {
        Ok(reader.into_inner())
    } else {
        Err(anyhow!(
            "HybridVsock trivial handshake failed. port: {}, response: {}",
            port,
            response
        ))
    }
}
