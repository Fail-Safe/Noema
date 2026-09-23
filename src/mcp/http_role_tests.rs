use super::*;
use rmcp::{
    model::{CallToolRequestParams, ClientInfo},
    transport::{
        StreamableHttpClientTransport, streamable_http_client::StreamableHttpClientTransportConfig,
    },
};

#[test]
fn role_profiles_are_explicit_and_preserve_continuity_profiles() {
    let full = NoemaServer::tool_router_for_profile("full").unwrap();
    let agent = NoemaServer::tool_router_for_profile("agent").unwrap();
    assert_eq!(full.map.len(), 35);
    assert_eq!(agent.map.len(), 24);
    for (role, count) in [("maintainer", 30), ("curator", 26), ("federation", 3)] {
        let router = NoemaServer::tool_router_for_profile(role).unwrap();
        assert_eq!(router.map.len(), count);
        assert!(router.map.keys().all(|name| full.map.contains_key(name)));
        if role != "federation" {
            assert!(agent.map.keys().all(|name| router.map.contains_key(name)));
        }
    }
    for name in [
        "sync_events",
        "tag_doctor",
        "record_consolidation_result",
        "announce_peer",
    ] {
        assert!(!agent.map.contains_key(name));
    }
    assert!(validate_http_tool_profile(true, "full").is_ok());
    assert!(validate_http_tool_profile(true, "continuity-read").is_err());
    assert!(validate_http_tool_profile(false, "continuity-read").is_ok());
    assert_eq!(
        NoemaServer::tool_router_for_profile("continuity-read")
            .unwrap()
            .map
            .len(),
        1
    );
    assert_eq!(
        NoemaServer::tool_router_for_profile("continuity-capture")
            .unwrap()
            .map
            .len(),
        2
    );
}

#[tokio::test]
async fn role_endpoints_enforce_discovery_calls_and_share_usage() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("test", temp.path()).unwrap();
    let root = temp.path().join("test");
    let server = NoemaServer::new("test", &root, false).unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let router = build_http_tool_router(&server, vec![address.to_string()]).unwrap();
    let router = apply_http_middleware(
        router,
        &AccessKey {
            value: "test-secret".into(),
            ..Default::default()
        },
    );
    let task = tokio::spawn(async move {
        axum::serve(listener, router).await.unwrap();
    });
    let http = reqwest::Client::new();
    let mut clients = Vec::new();
    for &(path, profile) in HTTP_TOOL_ENDPOINTS {
        let endpoint = format!("http://{address}{path}");
        assert_eq!(
            http.post(&endpoint).send().await.unwrap().status(),
            StatusCode::UNAUTHORIZED
        );
        let preflight = http
            .request(reqwest::Method::OPTIONS, &endpoint)
            .header(header::ORIGIN, "app://obsidian.md")
            .header(header::ACCESS_CONTROL_REQUEST_METHOD, "POST")
            .send()
            .await
            .unwrap();
        assert_eq!(preflight.status(), StatusCode::NO_CONTENT);
        for (name, value) in [
            (header::HOST, "untrusted.example"),
            (header::ORIGIN, "https://untrusted.example"),
        ] {
            let response = http
                .post(&endpoint)
                .bearer_auth("test-secret")
                .header(name, value)
                .send()
                .await
                .unwrap();
            assert_eq!(response.status(), StatusCode::FORBIDDEN);
        }

        let transport = StreamableHttpClientTransport::with_client(
            http.clone(),
            StreamableHttpClientTransportConfig::with_uri(endpoint).auth_header("test-secret"),
        );
        let client = ClientInfo::default().serve(transport).await.unwrap();
        let listed = client.list_all_tools().await.unwrap();
        let mut actual = listed
            .iter()
            .map(|t| t.name.to_string())
            .collect::<Vec<_>>();
        actual.sort();
        let mut expected = NoemaServer::tool_router_for_profile(profile)
            .unwrap()
            .map
            .keys()
            .map(ToString::to_string)
            .collect::<Vec<_>>();
        expected.sort();
        assert_eq!(actual, expected, "{profile}");
        assert!(
            listed
                .iter()
                .find(|t| t.name == "cortex_identity")
                .unwrap()
                .annotations
                .as_ref()
                .unwrap()
                .read_only_hint
                .unwrap()
        );
        let identity = client
            .call_tool(CallToolRequestParams::new("cortex_identity"))
            .await
            .unwrap();
        assert_ne!(identity.is_error, Some(true));
        if profile != "full" {
            // Even valid arguments cannot invoke an omitted tool by name.
            let result = client
                .call_tool(
                    CallToolRequestParams::new("announce_peer").with_arguments(
                        json!({"name":"peer-b","endpoint":"https://example.com"})
                            .as_object()
                            .unwrap()
                            .clone(),
                    ),
                )
                .await;
            assert!(
                result.unwrap_err().to_string().contains("tool not found"),
                "{profile}"
            );
        }
        if profile == "federation" {
            let result = client
                .call_tool(
                    CallToolRequestParams::new("create_trace").with_arguments(
                        json!({"title":"Forbidden", "type":"fact", "body":"must not persist"})
                            .as_object()
                            .unwrap()
                            .clone(),
                    ),
                )
                .await;
            assert!(result.unwrap_err().to_string().contains("tool not found"));
        }
        let allowed = match profile {
            "maintainer" => "metrics_summary",
            "curator" => "list_consolidation_candidates",
            "federation" => "sync_events",
            _ => "tag_stats",
        };
        assert_ne!(
            client
                .call_tool(CallToolRequestParams::new(allowed))
                .await
                .unwrap()
                .is_error,
            Some(true)
        );
        clients.push((profile, client));
    }
    let agent = &clients.iter().find(|(p, _)| *p == "agent").unwrap().1;
    let created = agent
        .call_tool(
            CallToolRequestParams::new("create_trace").with_arguments(
                json!({"title":"Shared role memory","type":"fact","body":"quartz"})
                    .as_object()
                    .unwrap()
                    .clone(),
            ),
        )
        .await
        .unwrap();
    assert_ne!(created.is_error, Some(true));
    let id = {
        let cx = server.open().await.unwrap();
        cx.list(&ListOptions::default()).unwrap()[0].id.clone()
    };
    for (profile, client) in &clients {
        if *profile == "federation" {
            continue;
        }
        let read = client
            .call_tool(
                CallToolRequestParams::new("get_trace").with_arguments(
                    json!({"id":id,"record_usage":true})
                        .as_object()
                        .unwrap()
                        .clone(),
                ),
            )
            .await
            .unwrap();
        assert_ne!(read.is_error, Some(true));
        let usage = client
            .call_tool(CallToolRequestParams::new("cortex_usage"))
            .await
            .unwrap();
        let output = usage.structured_content.unwrap();
        assert_eq!(output["runtime"]["mcp_tool_profile"], *profile);
        let directory = &output["runtime"]["mcp_endpoints"];
        assert_eq!(directory["transport"], "http");
        let endpoints = directory["endpoints"].as_array().unwrap();
        assert_eq!(endpoints.len(), 5);
        assert_eq!(
            endpoints
                .iter()
                .filter(|e| e["current_endpoint"] == true)
                .count(),
            1
        );
        let full = endpoints.iter().find(|e| e["profile"] == "full").unwrap();
        assert_eq!(
            full["additional_tool_count"],
            35 - NoemaServer::tool_router_for_profile(profile)
                .unwrap()
                .map
                .len()
        );
        let instructions = client
            .call_tool(CallToolRequestParams::new("get_instructions"))
            .await
            .unwrap();
        let text = serde_json::to_string(&instructions.content).unwrap();
        assert!(text.contains("/mcp/maintainer"));
        assert!(text.contains("/mcp/curator"));
        assert!(text.contains("Only tools advertised by this connection are callable here"));
    }
    {
        let cx = server.open().await.unwrap();
        let usage = cx.local_usage_since("", 100).unwrap();
        assert_eq!(
            usage.iter().find(|u| u.trace_id == id).unwrap().read_count,
            4
        );
    }
    for path in ["/mcp/agnt", "/mcp/agent/unknown", "/mcp/full"] {
        assert_eq!(
            http.post(format!("http://{address}{path}"))
                .bearer_auth("test-secret")
                .send()
                .await
                .unwrap()
                .status(),
            StatusCode::NOT_FOUND
        );
    }
    for (_, client) in clients {
        client.cancel().await.unwrap();
    }
    task.abort();
}

#[tokio::test]
async fn sessions_cannot_cross_role_endpoints() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("test", temp.path()).unwrap();
    let server = NoemaServer::new("test", temp.path().join("test"), false).unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let router = build_http_tool_router(&server, vec![address.to_string()]).unwrap();
    let task = tokio::spawn(async move {
        axum::serve(listener, router).await.unwrap();
    });
    let http = reqwest::Client::new();
    for (source, destination) in [("/mcp", "/mcp/agent"), ("/mcp/agent/", "/mcp")] {
        let response = http.post(format!("http://{address}{source}"))
            .header(header::ACCEPT, "application/json, text/event-stream")
            .json(&json!({"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}))
            .send().await.unwrap();
        assert_eq!(response.status(), StatusCode::OK);
        let session = response.headers()["mcp-session-id"].clone();
        response.text().await.unwrap();
        let response = http
            .post(format!("http://{address}{destination}"))
            .header(header::ACCEPT, "application/json, text/event-stream")
            .header("mcp-session-id", session)
            .json(&json!({"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}))
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }
    task.abort();
}

#[test]
fn stdio_endpoint_guidance_does_not_claim_an_http_connection() {
    let temp = tempfile::tempdir().unwrap();
    Cortex::create("test", temp.path()).unwrap();
    let mut server = NoemaServer::new("test", temp.path().join("test"), false).unwrap();
    server.tool_profile = "agent".into();
    server.tool_router = NoemaServer::tool_router_for_profile("agent").unwrap();
    let (text, directory) = server.endpoint_guidance();
    assert!(text.contains("If this cortex is also served over HTTP"));
    assert_eq!(directory["transport"], "stdio");
    let endpoints = directory["endpoints"].as_array().unwrap();
    assert!(endpoints.iter().all(|e| e["current_endpoint"] == false));
    assert_eq!(
        endpoints
            .iter()
            .find(|e| e["profile"] == "maintainer")
            .unwrap()["additional_tool_count"],
        6
    );
    assert_eq!(
        endpoints
            .iter()
            .find(|e| e["profile"] == "curator")
            .unwrap()["additional_tool_count"],
        2
    );
}
