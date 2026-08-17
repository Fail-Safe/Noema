#!/usr/bin/env python3
"""Render the public Rust rewrite report charts from curated benchmark data."""

from __future__ import annotations

import json
import os
from pathlib import Path
import tempfile

os.environ.setdefault(
    "MPLCONFIGDIR", str(Path(tempfile.gettempdir()) / "noema-matplotlib")
)
os.environ.setdefault(
    "XDG_CACHE_HOME", str(Path(tempfile.gettempdir()) / "noema-report-cache")
)

import matplotlib as mpl
import matplotlib.pyplot as plt
from matplotlib.patches import Patch
import numpy as np


ROOT = Path(__file__).resolve().parents[2]
DATA_PATH = Path(__file__).with_name("report-data.json")
OUTPUT_DIR = ROOT / "docs" / "assets" / "rust-rewrite"

GO = "#4477AA"
RUST = "#EE6677"
STANDARD = "#228833"
STRONG = "#AA3377"
NEUTRAL = "#6B7280"
GRID = "#D1D5DB"


def configure() -> None:
    mpl.rcParams.update(
        {
            "axes.spines.top": False,
            "axes.spines.right": False,
            "axes.titleweight": "bold",
            "axes.titlesize": 14,
            "axes.labelsize": 10,
            "figure.dpi": 140,
            "font.family": "DejaVu Sans",
            "font.size": 10,
            "legend.frameon": False,
            "savefig.facecolor": "white",
            "svg.fonttype": "none",
            "svg.hashsalt": "noema-rust-report-v1",
        }
    )


def save(fig: plt.Figure, name: str) -> None:
    OUTPUT_DIR.mkdir(parents=True, exist_ok=True)
    output_path = OUTPUT_DIR / name
    fig.savefig(
        output_path,
        format="svg",
        bbox_inches="tight",
        metadata={"Date": None, "Creator": "Noema Rust rewrite report generator"},
    )
    svg = output_path.read_text()
    output_path.write_text("\n".join(line.rstrip() for line in svg.splitlines()) + "\n")
    if preview := os.environ.get("NOEMA_REPORT_PREVIEW_DIR"):
        preview_dir = Path(preview)
        preview_dir.mkdir(parents=True, exist_ok=True)
        fig.savefig(preview_dir / f"{Path(name).stem}.png", bbox_inches="tight")
    plt.close(fig)


def grouped_bars(
    ax: plt.Axes,
    labels: list[str],
    go_values: list[float],
    rust_values: list[float],
    ylabel: str,
    value_format: str,
) -> None:
    x = np.arange(len(labels))
    width = 0.34
    go_bars = ax.bar(x - width / 2, go_values, width, label="Go", color=GO)
    rust_bars = ax.bar(x + width / 2, rust_values, width, label="Rust", color=RUST)
    ax.set_xticks(x, labels)
    ax.set_ylabel(ylabel)
    ax.set_ylim(bottom=0)
    ax.grid(axis="y", color=GRID, linewidth=0.7, alpha=0.7)
    ax.set_axisbelow(True)
    ax.legend(ncol=2, loc="upper left")
    ax.bar_label(go_bars, labels=[value_format.format(v) for v in go_values], padding=3)
    ax.bar_label(rust_bars, labels=[value_format.format(v) for v in rust_values], padding=3)


def render_mixed_scale(data: dict) -> None:
    scales = data["scale"]
    labels = [f'{entry["traces"] // 1000}k traces' for entry in scales]
    mixed = [entry["workloads"]["mixed_selective_read_write"] for entry in scales]

    fig, axes = plt.subplots(1, 2, figsize=(12.5, 4.4))
    grouped_bars(
        axes[0],
        labels,
        [entry["go_ops_per_second"] for entry in mixed],
        [entry["rust_ops_per_second"] for entry in mixed],
        "Operations per second",
        "{:,.0f}",
    )
    axes[0].set_title("Rust sustains higher mixed-workload throughput", fontsize=12)

    grouped_bars(
        axes[1],
        labels,
        [entry["go_rss_kib"] / 1024 for entry in mixed],
        [entry["rust_rss_kib"] / 1024 for entry in mixed],
        "Peak sampled RSS (MiB)",
        "{:.0f}",
    )
    axes[1].set_title("Rust uses less memory as the corpus grows", fontsize=12)
    fig.suptitle("Mixed selective-read/write workload", fontsize=16, fontweight="bold")
    fig.subplots_adjust(wspace=0.32, top=0.78)
    save(fig, "mixed-scale.svg")


def render_workload_ratios(data: dict) -> None:
    fig, axes = plt.subplots(1, 2, figsize=(12, 6.4))
    entries: list[tuple[str, float, float]] = []
    for scale in data["scale"]:
        for workload in scale["workloads"].values():
            entries.append(
                (
                    f'{scale["traces"] // 1000}k  {workload["label"]}',
                    workload["rust_ops_per_second"] / workload["go_ops_per_second"],
                    workload["rust_rss_kib"] / workload["go_rss_kib"],
                )
            )

    labels = [entry[0] for entry in entries]
    y = np.arange(len(entries))
    throughput = [entry[1] for entry in entries]
    memory = [entry[2] for entry in entries]

    axes[0].barh(y, throughput, color=RUST)
    axes[0].axvline(1, color=NEUTRAL, linewidth=1.2)
    axes[0].set_yticks(y, labels)
    axes[0].invert_yaxis()
    axes[0].set_xlim(0, 2.2)
    axes[0].set_xlabel("Rust throughput / Go throughput")
    axes[0].set_title("Rust is faster in every final scenario")
    axes[0].grid(axis="x", color=GRID, linewidth=0.7, alpha=0.7)
    for index, value in enumerate(throughput):
        axes[0].text(value + 0.03, index, f"{value:.2f}x", va="center")

    memory_colors = [RUST if value <= 1 else STRONG for value in memory]
    axes[1].barh(y, memory, color=memory_colors)
    axes[1].axvline(1, color=NEUTRAL, linewidth=1.2)
    axes[1].set_yticks(y, [""] * len(labels))
    axes[1].invert_yaxis()
    axes[1].set_xlim(0, 2.35)
    axes[1].set_xlabel("Rust RSS / Go RSS")
    axes[1].set_title("Memory gains depend on result shape")
    axes[1].grid(axis="x", color=GRID, linewidth=0.7, alpha=0.7)
    for index, value in enumerate(memory):
        axes[1].text(value + 0.03, index, f"{value:.2f}x", va="center")

    fig.suptitle("Final workload ratios (lower memory is better)", fontsize=16, fontweight="bold")
    fig.subplots_adjust(wspace=0.08, top=0.84)
    save(fig, "workload-ratios.svg")


def render_durability(data: dict) -> None:
    values = data["durability_10k"]
    fig, axes = plt.subplots(1, 3, figsize=(13.5, 4.8))
    metrics = [
        ("seed_seconds", "10k seed time", "Seconds", "{:.1f}"),
        ("mixed_ops_per_second", "Mixed throughput", "Operations per second", "{:,.0f}"),
        ("median_write_latency_ms", "Median write latency", "Milliseconds", "{:.3f}"),
    ]
    for ax, (key, title, ylabel, formatter) in zip(axes, metrics, strict=True):
        x = np.arange(2)
        width = 0.34
        go_values = [values["go_standard_control"][key], values["go_strong_control"][key]]
        rust_values = [values["rust_standard"][key], values["rust_strong"][key]]
        go_bars = ax.bar(x - width / 2, go_values, width, color=GO)
        rust_bars = ax.bar(x + width / 2, rust_values, width, color=[STANDARD, STRONG])
        ax.set_title(title)
        ax.set_ylabel(ylabel)
        ax.set_ylim(bottom=0)
        ax.set_xticks(x, ["Standard", "Strong"])
        ax.grid(axis="y", color=GRID, linewidth=0.7, alpha=0.7)
        ax.set_axisbelow(True)
        ax.bar_label(go_bars, labels=[formatter.format(value) for value in go_values], padding=3)
        ax.bar_label(rust_bars, labels=[formatter.format(value) for value in rust_values], padding=3)

    fig.suptitle(
        "Strong durability exchanges write performance for recovery guarantees",
        fontsize=16,
        fontweight="bold",
    )
    fig.legend(
        handles=[
            Patch(facecolor=GO, label="Paired Go control"),
            Patch(facecolor=STANDARD, label="Rust standard"),
            Patch(facecolor=STRONG, label="Rust strong"),
        ],
        loc="lower center",
        ncol=3,
        bbox_to_anchor=(0.5, 0.01),
    )
    fig.subplots_adjust(wspace=0.38, top=0.79, bottom=0.18)
    save(fig, "durability-tradeoff.svg")


def render_quality(data: dict) -> None:
    quality = data["quality"]
    labels = [entry["label"] for entry in quality]
    go_scores = [100 * entry["go_score"] / entry["maximum_score"] for entry in quality]
    rust_scores = [100 * entry["rust_score"] / entry["maximum_score"] for entry in quality]

    fig, ax = plt.subplots(figsize=(7.8, 4.6))
    grouped_bars(ax, labels, go_scores, rust_scores, "Share of maximum blind-review score", "{:.1f}%")
    ax.set_ylim(0, 105)
    ax.set_title("Blinded consolidation quality is effectively tied")
    ax.legend(ncol=1, loc="upper left", bbox_to_anchor=(1.01, 1))
    fig.subplots_adjust(right=0.84, top=0.85)
    save(fig, "quality-parity.svg")


def render_federation(data: dict) -> None:
    soak = data["federation_soak"]
    labels = ["Peak RSS", "Mean CPU"]
    go_values = [100, 100]
    rust_values = [
        100 * soak["rust"]["peak_rss_mib"] / soak["go"]["peak_rss_mib"],
        100 * soak["rust"]["mean_cpu_percent"] / soak["go"]["mean_cpu_percent"],
    ]

    fig, ax = plt.subplots(figsize=(7.6, 4.6))
    grouped_bars(ax, labels, go_values, rust_values, "Go baseline (%)", "{:.1f}%")
    ax.set_ylim(0, 115)
    ax.set_title("Rust used fewer resources in the bounded federation soak")
    fig.subplots_adjust(top=0.85)
    save(fig, "federation-resources.svg")


def render_artifact_size(data: dict) -> None:
    sizes = [data["artifacts"][name]["bytes"] / (1024 * 1024) for name in ("go", "rust")]
    fig, ax = plt.subplots(figsize=(6.4, 4.2))
    bars = ax.bar(["Go", "Rust"], sizes, color=[GO, RUST], width=0.55)
    ax.set_ylabel("Release binary size (MiB)")
    ax.set_ylim(bottom=0)
    ax.set_title("The Rust release artifact is 27.8% larger")
    ax.grid(axis="y", color=GRID, linewidth=0.7, alpha=0.7)
    ax.set_axisbelow(True)
    ax.bar_label(bars, labels=[f"{value:.1f} MiB" for value in sizes], padding=3)
    save(fig, "artifact-size.svg")


def main() -> None:
    configure()
    data = json.loads(DATA_PATH.read_text())
    render_mixed_scale(data)
    render_workload_ratios(data)
    render_durability(data)
    render_quality(data)
    render_federation(data)
    render_artifact_size(data)


if __name__ == "__main__":
    main()
