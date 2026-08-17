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
FAVORABLE = "#4477AA"
REGRESSION = "#EE7733"


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
    fig, axes = plt.subplots(1, 2, figsize=(13.5, 6.8), sharey=True)
    entries: list[tuple[str, float, float]] = []
    for scale in data["scale"]:
        for workload in scale["workloads"].values():
            entries.append(
                (
                    f'{scale["traces"] // 1000}k  {workload["label"]}',
                    100
                    * (
                        workload["rust_ops_per_second"]
                        / workload["go_ops_per_second"]
                        - 1
                    ),
                    100
                    * (
                        1
                        - workload["rust_rss_kib"]
                        / workload["go_rss_kib"]
                    ),
                )
            )

    labels = [entry[0] for entry in entries]
    y = np.arange(len(entries))
    throughput_advantage = [entry[1] for entry in entries]
    memory_advantage = [entry[2] for entry in entries]

    def colors(values: list[float]) -> list[str]:
        return [
            NEUTRAL if abs(value) < 0.5 else FAVORABLE if value > 0 else REGRESSION
            for value in values
        ]

    def label(ax: plt.Axes, index: int, value: float, text: str) -> None:
        if abs(value) < 0.5:
            ax.text(3, index, text, va="center", ha="left", fontweight="bold")
        elif value > 0:
            ax.text(value + 3, index, text, va="center", ha="left", fontweight="bold")
        else:
            ax.text(value - 3, index, text, va="center", ha="right", fontweight="bold")

    for ax in axes:
        ax.axvspan(-125, 0, color="#FFF7ED", zorder=0)
        ax.axvspan(0, 105, color="#EFF6FF", zorder=0)
        ax.axvline(0, color=NEUTRAL, linewidth=1.4)
        ax.set_xlim(-125, 105)
        ax.set_xticks([-100, -50, 0, 50, 100], ["100%", "50%", "same", "50%", "100%"])
        ax.grid(axis="x", color=GRID, linewidth=0.7, alpha=0.7)
        ax.set_axisbelow(True)
        ax.text(
            -62.5,
            1.035,
            "GO BETTER",
            transform=ax.get_xaxis_transform(),
            ha="center",
            color=REGRESSION,
            fontsize=9,
            fontweight="bold",
        )
        ax.text(
            52.5,
            1.035,
            "RUST BETTER",
            transform=ax.get_xaxis_transform(),
            ha="center",
            color=FAVORABLE,
            fontsize=9,
            fontweight="bold",
        )

    axes[0].barh(y, throughput_advantage, color=colors(throughput_advantage))
    axes[0].set_yticks(y, labels)
    axes[0].invert_yaxis()
    axes[0].set_title("Throughput", pad=22)
    axes[0].set_xlabel("Difference in operations per second")
    for index, value in enumerate(throughput_advantage):
        label(axes[0], index, value, f"{value:.0f}% faster")

    axes[1].barh(y, memory_advantage, color=colors(memory_advantage))
    axes[1].set_title("Peak memory", pad=22)
    axes[1].set_xlabel("Difference in peak sampled RSS")
    for index, value in enumerate(memory_advantage):
        if abs(value) < 0.5:
            text = "about equal"
        elif value > 0:
            text = f"{value:.0f}% less"
        else:
            text = f"{-value:.0f}% more"
        label(axes[1], index, value, text)

    fig.suptitle(
        "Rust is faster throughout; memory improves except for broad single-process scans",
        fontsize=16,
        fontweight="bold",
    )
    fig.text(
        0.5,
        0.02,
        "Blue bars are improvements; orange bars are regressions. Standard durability profile on the same benchmark corpus.",
        ha="center",
        fontsize=10,
        color=NEUTRAL,
    )
    fig.subplots_adjust(wspace=0.08, top=0.82, bottom=0.14)
    save(fig, "workload-ratios.svg")


def render_durability(data: dict) -> None:
    values = data["durability_10k"]
    fig, axes = plt.subplots(1, 3, figsize=(13.5, 4.8))
    profiles = [
        ("Go\ncurrent", values["go_current"], GO, 0.0),
        ("Rust\nstandard", values["rust_standard"], STANDARD, 1.0),
        ("Rust strong\n(opt-in only)", values["rust_strong"], STRONG, 2.7),
    ]
    metrics = [
        ("seed_seconds", "10k seed time", "Seconds", "{:.1f}"),
        ("mixed_ops_per_second", "Mixed throughput", "Operations per second", "{:,.0f}"),
        ("median_write_latency_ms", "Median write latency", "Milliseconds", "{:.3f}"),
    ]
    for ax, (key, title, ylabel, formatter) in zip(axes, metrics, strict=True):
        x = np.array([position for _, _, _, position in profiles])
        metric_values = [profile[key] for _, profile, _, _ in profiles]
        ax.axvspan(2.15, 3.25, color="#FDF2F8", zorder=0)
        ax.axvline(2.05, color=NEUTRAL, linewidth=1, linestyle="--")
        bars = ax.bar(
            x,
            metric_values,
            width=0.7,
            color=[color for _, _, color, _ in profiles],
            zorder=2,
        )
        ax.set_title(title)
        ax.set_ylabel(ylabel)
        ax.set_ylim(bottom=0)
        ax.set_xlim(-0.6, 3.3)
        ax.set_xticks(x, [label for label, _, _, _ in profiles])
        ax.grid(axis="y", color=GRID, linewidth=0.7, alpha=0.7)
        ax.set_axisbelow(True)
        ax.bar_label(
            bars,
            labels=[formatter.format(value) for value in metric_values],
            padding=3,
        )
        ax.text(
            2.7,
            0.98,
            "RUST-ONLY OPTION",
            transform=ax.get_xaxis_transform(),
            ha="center",
            va="top",
            fontsize=8,
            color=STRONG,
            fontweight="bold",
        )

    fig.suptitle(
        "Like-for-like default performance plus Rust's opt-in strong mode",
        fontsize=16,
        fontweight="bold",
    )
    fig.text(
        0.5,
        0.02,
        "Go has no strong mode; the separated bars show the cost of an additional Rust-only recovery protocol.",
        ha="center",
        fontsize=10,
        color=NEUTRAL,
        fontweight="bold",
    )
    fig.subplots_adjust(wspace=0.38, top=0.79, bottom=0.2)
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
