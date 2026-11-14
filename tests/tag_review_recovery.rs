#![cfg(debug_assertions)]

use std::{
    fs,
    process::{Command, Stdio},
    thread,
    time::{Duration, Instant},
};

use noema::{cortex::Cortex, trace::Trace};

#[test]
fn killed_review_apply_and_undo_restore_file_and_queue_even_with_standard_durability() {
    for undo in [false, true] {
        let temp = tempfile::tempdir().unwrap();
        let config = temp.path().join("config");
        let parent = temp.path().join("cortexes");
        let output = Command::new(env!("CARGO_BIN_EXE_noema"))
            .env("XDG_CONFIG_HOME", &config)
            .args([
                "init",
                "--name",
                "review-test",
                "--path",
                parent.to_str().unwrap(),
            ])
            .output()
            .unwrap();
        assert!(output.status.success());
        let root = parent.join("review-test");
        let cx = Cortex::open("review-test", &root).unwrap();
        let mut trace = Trace::new(
            "Review recovery",
            "note",
            "",
            vec!["rust".into()],
            "Concurrency notes",
        );
        cx.add(&mut trace).unwrap();
        let id = trace.frontmatter.id;
        let proposal = cx
            .propose_tag_review(
                &id,
                vec!["rust".into(), "concurrency".into()],
                "Body discusses concurrency",
                "fixture",
            )
            .unwrap()
            .unwrap();
        if undo {
            cx.apply_tag_review(&proposal.id, false).unwrap();
        }
        let path = cx.file_path(&cx.get(&id).unwrap());
        let bytes = fs::read(&path).unwrap();
        let tags = cx.get(&id).unwrap().tags;
        let history_len = cx.history(&id).unwrap().len();
        drop(cx);
        let marker = temp.path().join("review-paused");
        let mut child = Command::new(env!("CARGO_BIN_EXE_noema"))
            .env("XDG_CONFIG_HOME", &config)
            .env("NOEMA_DURABILITY", "standard")
            .env("NOEMA_RUST_TEST_PAUSE_AFTER_FILESYSTEM_MUTATION", &marker)
            .args([
                "--cortex",
                "review-test",
                "tags",
                "review",
                "decide",
                &proposal.id,
                if undo { "undo" } else { "apply" },
                "--yes",
            ])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(15);
        while !marker.exists() && Instant::now() < deadline {
            assert!(
                child.try_wait().unwrap().is_none(),
                "review child exited before fault boundary"
            );
            thread::sleep(Duration::from_millis(10));
        }
        if !marker.exists() {
            let _ = child.kill();
            let _ = child.wait();
            panic!("review child did not reach fault boundary");
        }
        assert_ne!(fs::read(&path).unwrap(), bytes);
        child.kill().unwrap();
        assert!(!child.wait().unwrap().success());
        let recovered = Cortex::open("review-test", &root).unwrap();
        assert_eq!(fs::read(&path).unwrap(), bytes);
        assert_eq!(recovered.get(&id).unwrap().tags, tags);
        assert_eq!(recovered.history(&id).unwrap().len(), history_len);
        assert_eq!(
            recovered.tag_review(&proposal.id).unwrap().status,
            if undo { "applied" } else { "pending" }
        );
        // The recovered proposal is still actionable, not stranded in a job state.
        recovered.apply_tag_review(&proposal.id, undo).unwrap();
    }
}
