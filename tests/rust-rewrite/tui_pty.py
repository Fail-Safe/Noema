#!/usr/bin/env python3
"""Exercise the Rust TUI inside a responsive pseudo-terminal."""

from __future__ import annotations

import argparse
import fcntl
import os
from pathlib import Path
import pty
import select
import signal
import struct
import subprocess
import tempfile
import termios
import time
from typing import Callable


ENTER_ALTERNATE = b"\x1b[?1049h"
LEAVE_ALTERNATE = b"\x1b[?1049l"
HIDE_CURSOR = b"\x1b[?25l"
SHOW_CURSOR = b"\x1b[?25h"
CURSOR_QUERY = b"\x1b[6n"
CURSOR_RESPONSE = b"\x1b[1;1R"


def environment(root: Path) -> dict[str, str]:
    env = os.environ.copy()
    env["HOME"] = str(root / "home")
    env["XDG_CONFIG_HOME"] = str(root / "config")
    env["TERM"] = "xterm-256color"
    env["COLORFGBG"] = "15;0"
    env["EDITOR"] = "/usr/bin/true"
    return env


def run(binary: Path, env: dict[str, str], *arguments: str) -> None:
    subprocess.run(
        [str(binary), *arguments],
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )


def set_window_size(fd: int, rows: int, columns: int) -> None:
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, columns, 0, 0))


def exercise(binary: Path, root: Path) -> None:
    env = environment(root)
    parent = root / "cortexes"
    run(binary, env, "init", "--name", "tui-live", "--path", str(parent))
    for title in ("Alpha live row", "Beta live row"):
        run(
            binary,
            env,
            "--cortex",
            "tui-live",
            "add",
            "--title",
            title,
            "--type",
            "fact",
            "--body",
            f"{title} body",
        )

    master, slave = pty.openpty()
    set_window_size(slave, 24, 80)
    initial = termios.tcgetattr(slave)
    process = subprocess.Popen(
        [str(binary), "--cortex", "tui-live", "tui"],
        env=env,
        stdin=slave,
        stdout=slave,
        stderr=slave,
        close_fds=True,
        start_new_session=True,
    )
    output = bytearray()
    answered_queries = 0

    def pump() -> None:
        nonlocal answered_queries
        ready, _, _ = select.select([master], [], [], 0.05)
        if ready:
            try:
                output.extend(os.read(master, 65_536))
            except OSError:
                pass
        queries = output.count(CURSOR_QUERY)
        while answered_queries < queries:
            os.write(master, CURSOR_RESPONSE)
            answered_queries += 1

    def wait_until(label: str, predicate: Callable[[], bool], timeout: float = 10) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            pump()
            if predicate():
                return
            if process.poll() is not None:
                raise RuntimeError(
                    f"TUI exited early with {process.returncode} while waiting for {label}"
                )
        raise RuntimeError(
            f"timed out waiting for {label}; terminal tail={bytes(output[-2000:])!r}"
        )

    try:
        wait_until(
            "initial alternate-screen render",
            lambda: ENTER_ALTERNATE in output
            and HIDE_CURSOR in output
            and b"j/k:nav" in output,
        )
        editor_offset = len(output)
        os.write(master, b"e")
        wait_until(
            "external-editor screen handoff",
            lambda: LEAVE_ALTERNATE in output[editor_offset:]
            and SHOW_CURSOR in output[editor_offset:]
            and ENTER_ALTERNATE in output[editor_offset:]
            and b"Updated " in output[editor_offset:],
        )
        editor_output = output[editor_offset:]
        if not (
            editor_output.index(LEAVE_ALTERNATE)
            < editor_output.index(SHOW_CURSOR)
            < editor_output.index(ENTER_ALTERNATE)
        ):
            raise RuntimeError("external-editor terminal transitions are out of order")
        os.write(master, b"?")
        wait_until("help overlay", lambda: b"Navigation" in output)
        os.write(master, b"?")
        before_resize = len(output)
        set_window_size(slave, 32, 120)
        os.killpg(process.pid, signal.SIGWINCH)
        wait_until(
            "32-row resize redraw", lambda: b"\x1b[32;1H" in output[before_resize:]
        )
        quit_offset = len(output)
        os.write(master, b"q")
        deadline = time.monotonic() + 10
        while process.poll() is None and time.monotonic() < deadline:
            pump()
        if process.poll() is None:
            terminal_flags = termios.tcgetattr(slave)[3]
            raise RuntimeError(
                "TUI did not exit after q; "
                f"cursor_queries={output.count(CURSOR_QUERY)}, "
                f"answered_queries={answered_queries}, "
                f"canonical={bool(terminal_flags & termios.ICANON)}, "
                f"echo={bool(terminal_flags & termios.ECHO)}, "
                f"terminal_tail={bytes(output[-2000:])!r}"
            )
        while select.select([master], [], [], 0)[0]:
            pump()
    finally:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()

    restored = termios.tcgetattr(slave)
    os.close(master)
    os.close(slave)
    if process.returncode != 0:
        raise RuntimeError(f"TUI exited with status {process.returncode}")
    if restored[3] & (termios.ICANON | termios.ECHO) != initial[3] & (
        termios.ICANON | termios.ECHO
    ):
        raise RuntimeError("TUI did not restore canonical/echo terminal modes")
    for label, sequence in [
        ("alternate-screen exit", LEAVE_ALTERNATE),
        ("cursor restore", SHOW_CURSOR),
    ]:
        if sequence not in output[quit_offset:]:
            raise RuntimeError(f"TUI output omitted {label}")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--rust", type=Path, required=True)
    args = parser.parse_args()
    if not args.rust.is_file():
        parser.error(f"binary does not exist: {args.rust}")
    with tempfile.TemporaryDirectory(prefix="noema-rust-tui-pty-") as directory:
        exercise(args.rust.resolve(), Path(directory))
    print(
        "Rust live TUI: render, editor handoff, help, resize, exit, "
        "terminal restore PASS"
    )


if __name__ == "__main__":
    main()
