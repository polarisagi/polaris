//! 极小 HTTP 探测：对 127.0.0.1 发一个 GET，只读状态行。
//!
//! 刻意不引 HTTP 客户端库：外壳需要的只有"状态码是多少"。reqwest / ureq 默认都
//! 带一整套 TLS 栈，而这里连的是本机回环、永远是明文 HTTP。为一件小事拉进几百个
//! 传递依赖，与 ADR-0095 决策二拒绝 sigstore-go 的理由是同一条。
//!
//! 只解析状态行、丢弃响应体，是因为调用方的判据只有三种：200（通）、
//! 401/403（凭证不对）、其他（当失败）。

use std::io::{Read, Write};
use std::net::TcpStream;
use std::time::Duration;

/// 探测结果。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProbeResult {
    /// 2xx
    Ok,
    /// 401 / 403——服务活着，但本次调用没通过鉴权
    Unauthorized,
    /// 其他状态码
    Status(u16),
    /// 连不上 / 超时 / 响应不可解析
    Unreachable,
}

/// 对 `127.0.0.1:port` 发 GET，可选带本地令牌。
pub fn get(port: u16, path: &str, token: Option<&str>) -> ProbeResult {
    let addr = format!("127.0.0.1:{port}");
    let timeout = Duration::from_secs(3);

    let socket = match addr.parse() {
        Ok(s) => s,
        Err(_) => return ProbeResult::Unreachable,
    };
    let mut stream = match TcpStream::connect_timeout(&socket, timeout) {
        Ok(s) => s,
        Err(_) => return ProbeResult::Unreachable,
    };
    let _ = stream.set_read_timeout(Some(timeout));
    let _ = stream.set_write_timeout(Some(timeout));

    // Host 写回环名：服务端的逃生阀分支要求 Host 为回环（防 DNS rebinding），
    // 写别的会让外壳在那条路径上表现得像一次重绑定攻击。
    let mut req = format!(
        "GET {path} HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\nConnection: close\r\nUser-Agent: polaris-desktop\r\n"
    );
    if let Some(t) = token {
        req.push_str(&format!("X-API-Key: {t}\r\n"));
    }
    req.push_str("\r\n");

    if stream.write_all(req.as_bytes()).is_err() {
        return ProbeResult::Unreachable;
    }

    // 只读够状态行的字节数即可，不等整个响应体。
    let mut buf = [0u8; 256];
    let n = match stream.read(&mut buf) {
        Ok(n) if n > 0 => n,
        _ => return ProbeResult::Unreachable,
    };
    parse_status(&buf[..n])
}

/// 对 `127.0.0.1:port` 发 GET 并读完整响应体，返回 (状态码, 响应体)。
///
/// 供 /healthz 取版本号与审批轮询使用。请求带 `Connection: close`，故读到 EOF
/// 即完整响应；响应体可能是 chunked（Go 的 net/http 在未预设 Content-Length
/// 且输出超过缓冲区时会切换），这里做最小的分块解码。
pub fn get_body(port: u16, path: &str, token: Option<&str>) -> Option<(u16, String)> {
    let socket = format!("127.0.0.1:{port}").parse().ok()?;
    let timeout = Duration::from_secs(5);
    let mut stream = TcpStream::connect_timeout(&socket, timeout).ok()?;
    let _ = stream.set_read_timeout(Some(timeout));
    let _ = stream.set_write_timeout(Some(timeout));

    let mut req = format!(
        "GET {path} HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\nConnection: close\r\nUser-Agent: polaris-desktop\r\n"
    );
    if let Some(t) = token {
        req.push_str(&format!("X-API-Key: {t}\r\n"));
    }
    req.push_str("\r\n");
    stream.write_all(req.as_bytes()).ok()?;

    let mut raw = Vec::new();
    // 上限 4 MiB：审批列表再长也到不了这个量级；不设上限的读取是一个
    // 可以被本机任意进程冒充守护进程、用无限响应拖垮外壳的口子。
    stream.take(4 << 20).read_to_end(&mut raw).ok()?;

    let split = raw.windows(4).position(|w| w == b"\r\n\r\n")?;
    let head = String::from_utf8_lossy(&raw[..split]).to_string();
    let body = &raw[split + 4..];
    let code = head.split_whitespace().nth(1)?.parse::<u16>().ok()?;

    let chunked = head.lines().any(|l| {
        l.to_ascii_lowercase().starts_with("transfer-encoding:")
            && l.to_ascii_lowercase().contains("chunked")
    });
    let body = if chunked {
        dechunk(body)?
    } else {
        body.to_vec()
    };
    Some((code, String::from_utf8_lossy(&body).to_string()))
}

/// 最小 HTTP/1.1 分块解码。格式错误返回 None——宁可这一轮拿不到数据，
/// 也不把半截 JSON 交给上层当成"空列表"。
fn dechunk(mut data: &[u8]) -> Option<Vec<u8>> {
    let mut out = Vec::new();
    loop {
        let line_end = data.windows(2).position(|w| w == b"\r\n")?;
        let size_str = String::from_utf8_lossy(&data[..line_end]);
        let size = usize::from_str_radix(size_str.split(';').next()?.trim(), 16).ok()?;
        data = &data[line_end + 2..];
        if size == 0 {
            return Some(out);
        }
        if data.len() < size + 2 {
            return None;
        }
        out.extend_from_slice(&data[..size]);
        data = &data[size + 2..];
    }
}

/// 从响应首行解析状态码。形如 `HTTP/1.1 200 OK`。
fn parse_status(bytes: &[u8]) -> ProbeResult {
    let head = String::from_utf8_lossy(bytes);
    let code = head
        .split_whitespace()
        .nth(1)
        .and_then(|c| c.parse::<u16>().ok());
    match code {
        Some(c) if (200..300).contains(&c) => ProbeResult::Ok,
        Some(401) | Some(403) => ProbeResult::Unauthorized,
        Some(c) => ProbeResult::Status(c),
        None => ProbeResult::Unreachable,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 状态行解析() {
        assert_eq!(parse_status(b"HTTP/1.1 200 OK\r\n"), ProbeResult::Ok);
        assert_eq!(
            parse_status(b"HTTP/1.1 204 No Content\r\n"),
            ProbeResult::Ok
        );
        assert_eq!(
            parse_status(b"HTTP/1.1 401 Unauthorized\r\n"),
            ProbeResult::Unauthorized
        );
        assert_eq!(
            parse_status(b"HTTP/1.1 403 Forbidden\r\n"),
            ProbeResult::Unauthorized
        );
        assert_eq!(
            parse_status(b"HTTP/1.1 500 Internal Server Error\r\n"),
            ProbeResult::Status(500)
        );
        assert_eq!(parse_status(b"garbage"), ProbeResult::Unreachable);
    }

    #[test]
    fn 分块解码() {
        let raw = b"4\r\nWiki\r\n5\r\npedia\r\n0\r\n\r\n";
        assert_eq!(dechunk(raw).unwrap(), b"Wikipedia");
        // 截断的分块必须失败，不能返回半截数据
        assert!(dechunk(b"9\r\nWiki").is_none());
    }

    /// 连不上的端口必须是 Unreachable，而不是 panic 或长时间挂起。
    #[test]
    fn 端口无人监听() {
        // 1 号端口在普通用户下不可能有服务。
        assert_eq!(get(1, "/healthz", None), ProbeResult::Unreachable);
    }
}
