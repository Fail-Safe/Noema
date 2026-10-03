use noema::embedding::HttpEmbedder;
use std::{
    io::{Read, Write},
    net::TcpListener,
    thread,
};

fn request(stream: &mut std::net::TcpStream) -> Vec<u8> {
    stream
        .set_read_timeout(Some(std::time::Duration::from_secs(10)))
        .unwrap();
    let mut data = Vec::new();
    loop {
        let mut buf = [0u8; 4096];
        let n = stream.read(&mut buf).unwrap();
        assert!(n > 0);
        data.extend_from_slice(&buf[..n]);
        if let Some(end) = data.windows(4).position(|x| x == b"\r\n\r\n") {
            let headers = String::from_utf8_lossy(&data[..end]);
            let length: usize = headers
                .lines()
                .find_map(|line| {
                    line.to_ascii_lowercase()
                        .strip_prefix("content-length:")
                        .map(|s| s.trim().parse().unwrap())
                })
                .unwrap_or(0);
            if data.len() >= end + 4 + length {
                return data;
            }
        }
    }
}

async fn check_redirect_is_refused(tokenizer: bool) {
    let initial = TcpListener::bind("127.0.0.1:0").unwrap();
    let destination = TcpListener::bind("127.0.0.1:0").unwrap();
    let first_port = initial.local_addr().unwrap().port();
    let second_port = destination.local_addr().unwrap().port();
    destination.set_nonblocking(true).unwrap();
    let first = thread::spawn(move || {
        let (mut stream, _) = initial.accept().unwrap();
        request(&mut stream);
        write!(stream, "HTTP/1.1 307 Temporary Redirect\r\nLocation: http://127.0.0.1:{second_port}/redirected\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
    });
    let second = thread::spawn(move || {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
        loop {
            match destination.accept() {
                Ok((mut stream, _)) => {
                    let data = request(&mut stream);
                    assert!(
                        String::from_utf8_lossy(&data).contains("synthetic-release-probe-input")
                    );
                    let body =
                        "{\"tokens\":[1,2],\"data\":[{\"index\":0,\"embedding\":[1.0,0.0,0.0]}]}";
                    write!(stream, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
                    return true;
                }
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                    if std::time::Instant::now() >= deadline {
                        return false;
                    }
                    thread::sleep(std::time::Duration::from_millis(10));
                }
                Err(error) => panic!("{error}"),
            }
        }
    });
    let client = HttpEmbedder::new(&format!("http://127.0.0.1:{first_port}/v1"), "").unwrap();
    let result = if tokenizer {
        client
            .token_count("synthetic-release-probe-input", "")
            .await
            .map(|_| ())
    } else {
        client
            .embed("test-model", &["synthetic-release-probe-input".to_owned()])
            .await
            .map(|_| ())
    };
    first.join().unwrap();
    let forwarded = second.join().unwrap();
    assert!(!forwarded, "redirect forwarded input to a different origin");
    assert!(
        result.is_err(),
        "redirect must be reported as a provider failure"
    );
}

#[tokio::test]
async fn tokenizer_redirect_does_not_forward_input() {
    check_redirect_is_refused(true).await;
}

#[tokio::test]
async fn embedding_redirect_does_not_forward_input() {
    check_redirect_is_refused(false).await;
}
