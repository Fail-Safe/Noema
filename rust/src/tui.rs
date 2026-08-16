use std::{
    collections::BTreeMap,
    fs, io,
    path::PathBuf,
    process::Command,
    time::{Duration, Instant},
};

use anyhow::{Context, Result, bail};
use crossterm::{
    cursor::Show,
    event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers},
    execute,
    terminal::{EnterAlternateScreen, LeaveAlternateScreen, disable_raw_mode, enable_raw_mode},
};
use ratatui::{
    Frame, Terminal,
    backend::CrosstermBackend,
    layout::{Alignment, Constraint, Direction, Layout, Rect},
    style::{Color, Modifier, Style},
    text::{Line, Span, Text},
    widgets::{Block, Borders, Cell as TableCell, Clear, Paragraph, Row as TableRow, Table},
};

use crate::{
    cortex::{Cortex, ListOptions, Row},
    trace::{Trace, VALID_TYPES},
};

const LIST_PERCENT: u16 = 34;
const FOLLOW_INTERVAL: Duration = Duration::from_secs(1);
const NEW_ROW_HIGHLIGHT_TICKS: u8 = 2;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Mode {
    List,
    Search,
    Confirm,
    Help,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum FocusPane {
    List,
    Detail,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum ConfirmAction {
    Archive,
    Trash,
    Purge,
}

impl ConfirmAction {
    fn label(self) -> &'static str {
        match self {
            Self::Archive => "archive",
            Self::Trash => "trash",
            Self::Purge => "purge",
        }
    }
}

#[derive(Debug, PartialEq, Eq)]
enum InputAction {
    Continue,
    Quit,
    EditNew,
    EditExisting(String),
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct FilterContext {
    query: String,
    show_all: bool,
    show_trashed: bool,
    visible_short: bool,
    visible_mid: bool,
    visible_long: bool,
}

struct Model<'a> {
    cortex: &'a Cortex,
    rows: Vec<Row>,
    cursor: usize,
    mode: Mode,
    focus: FocusPane,
    query: String,
    search_input: String,
    show_all: bool,
    show_trashed: bool,
    visible_short: bool,
    visible_mid: bool,
    visible_long: bool,
    confirm: Option<(ConfirmAction, String)>,
    detail_scroll: usize,
    session_votes: BTreeMap<String, i64>,
    new_row_ttl: BTreeMap<String, u8>,
    last_context: Option<FilterContext>,
    follow: bool,
    last_refresh: Instant,
    status: String,
    error: String,
    light: bool,
}

impl<'a> Model<'a> {
    fn new(cortex: &'a Cortex, theme: &str) -> Result<Self> {
        let mut model = Self {
            cortex,
            rows: Vec::new(),
            cursor: 0,
            mode: Mode::List,
            focus: FocusPane::List,
            query: String::new(),
            search_input: String::new(),
            show_all: false,
            show_trashed: false,
            visible_short: true,
            visible_mid: true,
            visible_long: false,
            confirm: None,
            detail_scroll: 0,
            session_votes: BTreeMap::new(),
            new_row_ttl: BTreeMap::new(),
            last_context: None,
            follow: false,
            last_refresh: Instant::now(),
            status: String::new(),
            error: String::new(),
            light: resolve_theme(theme) == "light",
        };
        model.reload()?;
        Ok(model)
    }

    fn selected_id(&self) -> Option<&str> {
        self.rows.get(self.cursor).map(|row| row.id.as_str())
    }

    fn selected_trace(&self) -> Option<Trace> {
        self.selected_id()
            .and_then(|id| self.cortex.get_trace(id).ok().map(|(_, trace)| trace))
    }

    fn tiers(&self) -> Option<Vec<String>> {
        if self.visible_short && self.visible_mid && self.visible_long {
            return None;
        }
        let mut tiers = Vec::new();
        if self.visible_short {
            tiers.push("short".into());
        }
        if self.visible_mid {
            tiers.push("mid".into());
        }
        if self.visible_long {
            tiers.push("long".into());
        }
        Some(tiers)
    }

    fn reload(&mut self) -> Result<()> {
        let selected = self.selected_id().map(str::to_owned);
        let context = self.filter_context();
        let Some(tiers) = self.tiers() else {
            return self.load_rows(selected, Vec::new(), context);
        };
        if tiers.is_empty() {
            self.rows.clear();
            self.cursor = 0;
            self.detail_scroll = 0;
            self.new_row_ttl.clear();
            self.last_context = Some(context);
            return Ok(());
        }
        self.load_rows(selected, tiers, context)
    }

    fn load_rows(
        &mut self,
        selected: Option<String>,
        tiers: Vec<String>,
        context: FilterContext,
    ) -> Result<()> {
        let options = ListOptions {
            tiers,
            trashed: self.show_trashed,
            all: self.show_all,
            ..Default::default()
        };
        let new_rows = if self.query.is_empty() {
            self.cortex.list(&options)?
        } else {
            self.cortex.search(&self.query, &options)?
        };
        if self.last_context.as_ref() == Some(&context) {
            self.new_row_ttl.retain(|_, ttl| {
                *ttl = ttl.saturating_sub(1);
                *ttl > 0
            });
            for row in &new_rows {
                if !self.rows.iter().any(|previous| previous.id == row.id) {
                    self.new_row_ttl
                        .insert(row.id.clone(), NEW_ROW_HIGHLIGHT_TICKS);
                }
            }
        } else {
            self.new_row_ttl.clear();
        }
        self.rows = new_rows;
        if let Some(selected) = selected
            && let Some(index) = self.rows.iter().position(|row| row.id == selected)
        {
            self.cursor = index;
        }
        self.cursor = self.cursor.min(self.rows.len().saturating_sub(1));
        self.last_context = Some(context);
        self.last_refresh = Instant::now();
        Ok(())
    }

    fn filter_context(&self) -> FilterContext {
        FilterContext {
            query: self.query.clone(),
            show_all: self.show_all,
            show_trashed: self.show_trashed,
            visible_short: self.visible_short,
            visible_mid: self.visible_mid,
            visible_long: self.visible_long,
        }
    }

    fn update(&mut self, key: KeyEvent) -> Result<InputAction> {
        self.error.clear();
        self.status.clear();
        match self.mode {
            Mode::List => self.update_list(key),
            Mode::Search => self.update_search(key),
            Mode::Confirm => self.update_confirm(key),
            Mode::Help => Ok(self.update_help(key)),
        }
    }

    fn update_list(&mut self, key: KeyEvent) -> Result<InputAction> {
        if key.modifiers.contains(KeyModifiers::CONTROL) && key.code == KeyCode::Char('c') {
            return Ok(InputAction::Quit);
        }
        if key.modifiers.contains(KeyModifiers::CONTROL)
            && matches!(key.code, KeyCode::Char('d') | KeyCode::Char('u'))
        {
            if self.focus == FocusPane::Detail {
                if key.code == KeyCode::Char('d') {
                    self.detail_scroll = self.detail_scroll.saturating_add(10);
                } else {
                    self.detail_scroll = self.detail_scroll.saturating_sub(10);
                }
            }
            return Ok(InputAction::Continue);
        }
        match key.code {
            KeyCode::Char('q') => return Ok(InputAction::Quit),
            KeyCode::Tab => {
                self.focus = match self.focus {
                    FocusPane::List => FocusPane::Detail,
                    FocusPane::Detail => FocusPane::List,
                };
            }
            KeyCode::Left => self.focus = FocusPane::List,
            KeyCode::Right => self.focus = FocusPane::Detail,
            KeyCode::Down | KeyCode::Char('j') => {
                if self.focus == FocusPane::Detail {
                    self.detail_scroll = self.detail_scroll.saturating_add(1);
                } else if self.cursor + 1 < self.rows.len() {
                    self.cursor += 1;
                    self.detail_scroll = 0;
                }
            }
            KeyCode::Up | KeyCode::Char('k') => {
                if self.focus == FocusPane::Detail {
                    self.detail_scroll = self.detail_scroll.saturating_sub(1);
                } else if self.cursor > 0 {
                    self.cursor -= 1;
                    self.detail_scroll = 0;
                }
            }
            KeyCode::Char('g') => {
                if self.focus == FocusPane::Detail {
                    self.detail_scroll = 0;
                } else {
                    self.cursor = 0;
                    self.detail_scroll = 0;
                }
            }
            KeyCode::Char('G') => {
                if self.focus == FocusPane::Detail {
                    self.detail_scroll = usize::MAX;
                } else {
                    self.cursor = self.rows.len().saturating_sub(1);
                    self.detail_scroll = 0;
                }
            }
            KeyCode::PageDown => {
                if self.focus == FocusPane::Detail {
                    self.detail_scroll = self.detail_scroll.saturating_add(10);
                }
            }
            KeyCode::PageUp => {
                if self.focus == FocusPane::Detail {
                    self.detail_scroll = self.detail_scroll.saturating_sub(10);
                }
            }
            KeyCode::Char('n') if !self.show_trashed => return Ok(InputAction::EditNew),
            KeyCode::Char('e') if !self.show_trashed => {
                if let Some(row) = self.rows.get(self.cursor) {
                    if row.source_locked && row.origin != self.cortex.name {
                        self.status = format!("Trace is source-locked by {}", row.origin);
                    } else {
                        return Ok(InputAction::EditExisting(row.id.clone()));
                    }
                }
            }
            KeyCode::Char('d') if !self.show_trashed => self.begin_confirm(ConfirmAction::Archive),
            KeyCode::Char('D') => self.begin_confirm(if self.show_trashed {
                ConfirmAction::Purge
            } else {
                ConfirmAction::Trash
            }),
            KeyCode::Char('u') if !self.show_trashed => {
                if let Some(id) = self.selected_id().map(str::to_owned) {
                    self.cortex.unarchive(&id)?;
                    self.status = format!("Unarchived {id}");
                    self.reload()?;
                }
            }
            KeyCode::Char('r') if self.show_trashed => {
                if let Some(id) = self.selected_id().map(str::to_owned) {
                    self.cortex.recover(&id)?;
                    self.status = format!("Recovered {id}");
                    self.reload()?;
                }
            }
            KeyCode::Char('a') => {
                self.show_all = !self.show_all;
                self.show_trashed = false;
                self.cursor = 0;
                self.reload()?;
            }
            KeyCode::Char('t') => {
                self.show_trashed = !self.show_trashed;
                self.show_all = false;
                self.cursor = 0;
                self.reload()?;
            }
            KeyCode::Char('1') => {
                self.visible_short = !self.visible_short;
                self.cursor = 0;
                self.reload()?;
            }
            KeyCode::Char('2') => {
                self.visible_mid = !self.visible_mid;
                self.cursor = 0;
                self.reload()?;
            }
            KeyCode::Char('3') => {
                self.visible_long = !self.visible_long;
                self.cursor = 0;
                self.reload()?;
            }
            KeyCode::Char('0') => {
                self.visible_short = true;
                self.visible_mid = true;
                self.visible_long = true;
                self.cursor = 0;
                self.reload()?;
            }
            KeyCode::Char('?') => self.mode = Mode::Help,
            KeyCode::Char('+') | KeyCode::Char('=') => self.cast_vote(1)?,
            KeyCode::Char('-') => self.cast_vote(-1)?,
            KeyCode::Char('/') => {
                self.mode = Mode::Search;
                self.search_input.clear();
            }
            KeyCode::Char('f') => {
                self.follow = !self.follow;
                self.last_refresh = Instant::now();
                self.status = format!("Live mode {}", if self.follow { "on" } else { "off" });
            }
            KeyCode::Char('R') => self.reload()?,
            KeyCode::Esc => {
                if self.focus == FocusPane::Detail {
                    self.focus = FocusPane::List;
                } else if !self.query.is_empty() {
                    self.query.clear();
                    self.cursor = 0;
                    self.reload()?;
                }
            }
            _ => {}
        }
        Ok(InputAction::Continue)
    }

    fn update_search(&mut self, key: KeyEvent) -> Result<InputAction> {
        match key.code {
            KeyCode::Enter => {
                self.query = self.search_input.clone();
                self.mode = Mode::List;
                self.cursor = 0;
                self.reload()?;
            }
            KeyCode::Esc => {
                self.mode = Mode::List;
                self.search_input = self.query.clone();
            }
            KeyCode::Backspace => {
                self.search_input.pop();
            }
            KeyCode::Char(character)
                if !key
                    .modifiers
                    .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT) =>
            {
                self.search_input.push(character);
                self.search_input = self.search_input.chars().take(120).collect();
            }
            _ => {}
        }
        Ok(InputAction::Continue)
    }

    fn update_confirm(&mut self, key: KeyEvent) -> Result<InputAction> {
        match key.code {
            KeyCode::Char('y') | KeyCode::Char('Y') | KeyCode::Enter => {
                let Some((action, id)) = self.confirm.take() else {
                    self.mode = Mode::List;
                    return Ok(InputAction::Continue);
                };
                match action {
                    ConfirmAction::Archive => self.cortex.archive(&id)?,
                    ConfirmAction::Trash => self.cortex.trash(&id)?,
                    ConfirmAction::Purge => self.cortex.remove_hard(&id)?,
                }
                self.mode = Mode::List;
                self.status = format!("{}d {id}", action.label());
                self.reload()?;
            }
            KeyCode::Char('n') | KeyCode::Char('N') | KeyCode::Esc => {
                self.confirm = None;
                self.mode = Mode::List;
            }
            _ => {}
        }
        Ok(InputAction::Continue)
    }

    fn update_help(&mut self, key: KeyEvent) -> InputAction {
        if key.code == KeyCode::Char('q')
            || (key.modifiers.contains(KeyModifiers::CONTROL) && key.code == KeyCode::Char('c'))
        {
            return InputAction::Quit;
        }
        if matches!(
            key.code,
            KeyCode::Char('?') | KeyCode::Char(' ') | KeyCode::Esc | KeyCode::Enter
        ) {
            self.mode = Mode::List;
        }
        InputAction::Continue
    }

    fn begin_confirm(&mut self, action: ConfirmAction) {
        if let Some(id) = self.selected_id().map(str::to_owned) {
            self.confirm = Some((action, id));
            self.mode = Mode::Confirm;
        }
    }

    fn cast_vote(&mut self, target: i64) -> Result<()> {
        let Some(id) = self.selected_id().map(str::to_owned) else {
            return Ok(());
        };
        let previous = self.session_votes.get(&id).copied().unwrap_or_default();
        let next = if previous == target { 0 } else { target };
        self.cortex.vote(&id, next - previous, "human")?;
        self.session_votes.insert(id.clone(), next);
        self.status = if next == 0 {
            format!("Cleared vote on {id}")
        } else {
            format!("{} {id}", if next > 0 { "▲" } else { "▼" })
        };
        Ok(())
    }

    fn render(&self, frame: &mut Frame) {
        let palette = Palette::new(self.light);
        let area = frame.area();
        frame.render_widget(Block::default().style(palette.surface), area);
        let vertical = Layout::default()
            .direction(Direction::Vertical)
            .constraints([
                Constraint::Length(1),
                Constraint::Min(1),
                Constraint::Length(1),
            ])
            .split(area);
        self.render_header(frame, vertical[0], palette);
        if self.mode == Mode::Help {
            self.render_help(frame, vertical[1], palette);
        } else {
            let panes = Layout::default()
                .direction(Direction::Horizontal)
                .constraints([
                    Constraint::Percentage(LIST_PERCENT),
                    Constraint::Percentage(100 - LIST_PERCENT),
                ])
                .split(vertical[1]);
            self.render_list(frame, panes[0], palette);
            self.render_detail(frame, panes[1], palette);
        }
        self.render_footer(frame, vertical[2], palette);
        if self.mode == Mode::Confirm {
            self.render_confirm(frame, area, palette);
        }
    }

    fn render_header(&self, frame: &mut Frame, area: Rect, palette: Palette) {
        let mut spans = vec![
            Span::styled("Noema", palette.brand.add_modifier(Modifier::BOLD)),
            Span::styled(".", palette.red.add_modifier(Modifier::BOLD)),
            Span::styled(format!("  {}", self.cortex.name), palette.dim),
        ];
        if !self.query.is_empty() {
            spans.push(Span::styled(
                format!("  search:\"{}\"", self.query),
                palette.dim,
            ));
        }
        if self.show_trashed {
            spans.push(Span::styled("  [trash]", palette.dim));
        } else if self.show_all {
            spans.push(Span::styled("  [all]", palette.dim));
        }
        if self.follow {
            spans.push(Span::styled(
                "  ● live",
                palette.success.add_modifier(Modifier::BOLD),
            ));
        }
        let count = format!("{} traces", self.rows.len());
        let count_width = count.chars().count().min(usize::from(area.width)) as u16;
        let sections = Layout::default()
            .direction(Direction::Horizontal)
            .constraints([
                Constraint::Min(0),
                Constraint::Length(count_width.saturating_add(1).min(area.width)),
            ])
            .split(area);
        frame.render_widget(
            Paragraph::new(Line::from(spans)).style(palette.surface),
            sections[0],
        );
        frame.render_widget(
            Paragraph::new(count)
                .style(palette.dim)
                .alignment(Alignment::Right),
            sections[1],
        );
    }

    fn render_list(&self, frame: &mut Frame, area: Rect, palette: Palette) {
        if self.rows.is_empty() {
            frame.render_widget(
                Paragraph::new("  No traces.")
                    .style(palette.dim)
                    .block(Block::default().borders(Borders::RIGHT)),
                area,
            );
            return;
        }
        let height = area.height as usize;
        let title_width = list_title_width(area.width);
        let start = if self.cursor >= height {
            self.cursor - height + 1
        } else {
            0
        };
        let rows = self
            .rows
            .iter()
            .enumerate()
            .skip(start)
            .take(height)
            .map(|(index, row)| {
                let cursor = if index == self.cursor {
                    if self.focus == FocusPane::List {
                        "▸"
                    } else {
                        "·"
                    }
                } else {
                    " "
                };
                let date = row.created_at.get(..10).unwrap_or(&row.created_at);
                let dimmed = self.focus == FocusPane::Detail;
                let style = if index == self.cursor {
                    if dimmed {
                        palette.selected_dim
                    } else {
                        palette.selected
                    }
                } else if self.new_row_ttl.get(&row.id).copied().unwrap_or_default() > 0 {
                    if dimmed {
                        palette.new_dim
                    } else {
                        palette.new_row
                    }
                } else if dimmed {
                    palette.row_dim
                } else {
                    palette.surface
                };
                let mut title = row.title.clone();
                if !row.trashed_at.is_empty() || !row.archived_at.is_empty() {
                    title.insert(0, '~');
                }
                TableRow::new(vec![
                    TableCell::from(Line::from(vec![
                        Span::raw(format!("{cursor} ")),
                        Span::styled(
                            tier_badge(&row.tier),
                            tier_style(&row.tier, dimmed, palette),
                        ),
                    ])),
                    TableCell::from(truncate(&title, title_width)),
                    TableCell::from(format!("[{}]", row.trace_type)),
                    TableCell::from(date.to_owned()),
                ])
                .style(style)
            });
        frame.render_widget(
            Table::new(
                rows,
                [
                    Constraint::Length(4),
                    Constraint::Min(6),
                    Constraint::Length(14),
                    Constraint::Length(10),
                ],
            )
            .column_spacing(1)
            .block(Block::default().borders(Borders::RIGHT)),
            area,
        );
    }

    fn render_detail(&self, frame: &mut Frame, area: Rect, palette: Palette) {
        let Some(row) = self.rows.get(self.cursor) else {
            frame.render_widget(
                Paragraph::new("  Select a trace to preview.").style(palette.dim),
                area,
            );
            return;
        };
        let Some(trace) = self.selected_trace() else {
            frame.render_widget(
                Paragraph::new("  Trace content unavailable.").style(palette.error),
                area,
            );
            return;
        };
        let votes = self.cortex.tier_votes(&row.id).unwrap_or_default();
        let tier = if row.tier.is_empty() {
            "short"
        } else {
            row.tier.as_str()
        };
        let value_width = area.width.saturating_sub(12).max(4) as usize;
        let tier_label = format!("{tier}  (votes: {})", signed_count(votes));
        let mut lines = vec![
            metadata("id", &truncate(&row.id, value_width), palette),
            metadata("title", &truncate(&row.title, value_width), palette),
            metadata_chip("type", &row.trace_type, palette),
            metadata("tier", &truncate(&tier_label, value_width), palette),
        ];
        if !row.author.is_empty() {
            lines.push(metadata(
                "author",
                &truncate(&row.author, value_width),
                palette,
            ));
        }
        if !row.tags.is_empty() {
            lines.extend(metadata_tag_chips(&row.tags, value_width, palette));
        }
        lines.push(metadata(
            "created",
            trace
                .frontmatter
                .created
                .get(..10)
                .unwrap_or(&trace.frontmatter.created),
            palette,
        ));
        let body_width = area.width.saturating_sub(4).max(10) as usize;
        let mut body_lines = Vec::new();
        for line in trace.body.lines() {
            for wrapped in wrap_line(line, body_width) {
                body_lines.push(Line::from(format!("  {wrapped}")));
            }
        }
        let visible_body_height = usize::from(area.height).saturating_sub(lines.len() + 1);
        let maximum = body_lines.len().saturating_sub(visible_body_height);
        let scroll = self.detail_scroll.min(maximum);
        lines.push(detail_separator(
            area.width,
            scroll,
            visible_body_height,
            body_lines.len(),
            palette,
        ));
        if body_lines.is_empty() {
            lines.push(Line::from(Span::styled("  (no body)", palette.dim)));
        } else {
            lines.extend(
                body_lines
                    .into_iter()
                    .skip(scroll)
                    .take(visible_body_height),
            );
        }
        frame.render_widget(
            Paragraph::new(Text::from(lines)).style(palette.surface),
            area,
        );
    }

    fn render_footer(&self, frame: &mut Frame, area: Rect, palette: Palette) {
        let (text, style) = match self.mode {
            Mode::Search => (format!(" /{}", self.search_input), palette.surface),
            Mode::Confirm => (String::new(), palette.surface),
            Mode::Help => ("  ?/esc/enter:back  q:quit".into(), palette.dim),
            Mode::List if !self.error.is_empty() => {
                (format!("  {}", self.error), palette.error)
            }
            Mode::List if !self.status.is_empty() => {
                (format!("  {}", self.status), palette.success)
            }
            Mode::List if self.focus == FocusPane::Detail => (
                "  j/k:scroll  g/G:top/bot  PgUp/PgDn:half  ←/tab:list  esc:list  ?:help  q:quit".into(),
                palette.dim,
            ),
            Mode::List if self.show_trashed => (
                "  j/k:nav  r:recover  D:purge  t:back  /:search  →/tab:body  f:live  R:refresh  ?:help  q:quit".into(),
                palette.dim,
            ),
            Mode::List => (
                "  j/k:nav  n:new  e:edit  d:archive  D:trash  1/2/3:tier  0:all-tiers  /:search  →/tab:body  ?:help  q:quit".into(),
                palette.dim,
            ),
        };
        frame.render_widget(Paragraph::new(text).style(style), area);
    }

    fn render_help(&self, frame: &mut Frame, area: Rect, palette: Palette) {
        let help = [
            "",
            "  Noema.  keybindings",
            "",
            "  Navigation",
            "    j / k or ↓ / ↑    move cursor / scroll detail",
            "    g / G             top / bottom",
            "    → / ← or tab      focus detail / list pane",
            "",
            "  Trace actions",
            "    n / e              new / edit",
            "    + / -              cycle tier preference vote",
            "    d / u              archive / unarchive",
            "    D / r              trash or purge / recover",
            "",
            "  Views & filters",
            "    1 / 2 / 3 / 0      tier visibility / all tiers",
            "    a / t              archived / trash view",
            "    /                  search",
            "    f / R              live follow / refresh",
            "",
            "  ? / esc / enter      close help",
            "  q / ctrl+c           quit",
        ]
        .join("\n");
        frame.render_widget(Paragraph::new(help).style(palette.surface), area);
    }

    fn render_confirm(&self, frame: &mut Frame, area: Rect, palette: Palette) {
        let Some((action, id)) = self.confirm.as_ref() else {
            return;
        };
        let popup = centered_rect(70, 3, area);
        frame.render_widget(Clear, popup);
        frame.render_widget(
            Paragraph::new(format!(" {} {:?}? [y/N] ", action.label(), id))
                .style(palette.error)
                .block(Block::default().borders(Borders::ALL).style(palette.red)),
            popup,
        );
    }
}

#[derive(Clone, Copy)]
struct Palette {
    surface: Style,
    brand: Style,
    red: Style,
    dim: Style,
    divider: Style,
    selected: Style,
    selected_dim: Style,
    row_dim: Style,
    new_row: Style,
    new_dim: Style,
    chip: Style,
    tier_short: Style,
    tier_mid: Style,
    tier_long: Style,
    success: Style,
    error: Style,
}

impl Palette {
    fn new(light: bool) -> Self {
        let (background, foreground, dim, selected_bg, selected_fg) = if light {
            (
                Color::Rgb(255, 255, 255),
                Color::Rgb(26, 26, 26),
                Color::Indexed(243),
                Color::Rgb(236, 228, 212),
                Color::Indexed(232),
            )
        } else {
            (
                Color::Rgb(26, 26, 26),
                Color::Rgb(236, 228, 212),
                Color::Indexed(240),
                Color::Indexed(236),
                Color::Indexed(255),
            )
        };
        Self {
            surface: Style::default().bg(background).fg(foreground),
            brand: Style::default().bg(background).fg(foreground),
            red: Style::default().bg(background).fg(Color::Rgb(225, 0, 50)),
            dim: Style::default().bg(background).fg(dim),
            divider: Style::default().bg(background).fg(Color::Indexed(238)),
            selected: Style::default()
                .bg(selected_bg)
                .fg(selected_fg)
                .add_modifier(Modifier::BOLD),
            selected_dim: Style::default().bg(background).fg(Color::Indexed(244)),
            row_dim: Style::default().bg(background).fg(if light {
                Color::Indexed(249)
            } else {
                Color::Indexed(238)
            }),
            new_row: Style::default().bg(background).fg(if light {
                Color::Indexed(22)
            } else {
                Color::Indexed(71)
            }),
            new_dim: Style::default().bg(background).fg(if light {
                Color::Indexed(28)
            } else {
                Color::Indexed(65)
            }),
            chip: Style::default().bg(foreground).fg(background),
            tier_short: Style::default()
                .fg(Color::Indexed(if light { 166 } else { 208 }))
                .add_modifier(Modifier::BOLD),
            tier_mid: Style::default()
                .fg(Color::Indexed(if light { 136 } else { 221 }))
                .add_modifier(Modifier::BOLD),
            tier_long: Style::default()
                .fg(Color::Indexed(if light { 25 } else { 39 }))
                .add_modifier(Modifier::BOLD),
            success: Style::default().bg(background).fg(if light {
                Color::Indexed(22)
            } else {
                Color::Indexed(71)
            }),
            error: Style::default().bg(background).fg(if light {
                Color::Indexed(124)
            } else {
                Color::Indexed(196)
            }),
        }
    }
}

struct TerminalGuard;

impl Drop for TerminalGuard {
    fn drop(&mut self) {
        let _ = disable_raw_mode();
        let _ = execute!(io::stdout(), LeaveAlternateScreen, Show);
    }
}

pub fn run(cortex: &Cortex, theme: &str) -> Result<()> {
    let mut model = Model::new(cortex, theme)?;
    enable_raw_mode()?;
    let _guard = TerminalGuard;
    let mut stdout = io::stdout();
    execute!(stdout, EnterAlternateScreen)?;
    let mut terminal = Terminal::new(CrosstermBackend::new(stdout))?;
    terminal.clear()?;

    loop {
        terminal.draw(|frame| model.render(frame))?;
        if model.follow
            && model.last_refresh.elapsed() >= FOLLOW_INTERVAL
            && let Err(error) = model.reload()
        {
            model.error = error.to_string();
        }
        if !event::poll(Duration::from_millis(100))? {
            continue;
        }
        let Event::Key(key) = event::read()? else {
            continue;
        };
        if !matches!(key.kind, KeyEventKind::Press | KeyEventKind::Repeat) {
            continue;
        }
        match model.update(key) {
            Ok(InputAction::Continue) => {}
            Ok(InputAction::Quit) => break,
            Ok(InputAction::EditNew) => {
                if let Err(error) = edit_new(&mut terminal, &mut model) {
                    model.error = error.to_string();
                }
            }
            Ok(InputAction::EditExisting(id)) => {
                if let Err(error) = edit_existing(&mut terminal, &mut model, &id) {
                    model.error = error.to_string();
                }
            }
            Err(error) => model.error = error.to_string(),
        }
    }
    Ok(())
}

fn edit_new(
    terminal: &mut Terminal<CrosstermBackend<io::Stdout>>,
    model: &mut Model<'_>,
) -> Result<()> {
    let path = new_trace_path();
    fs::write(
        &path,
        "---\ntitle: \"\"\ntype: note\nauthor: \"\"\ntags: []\n---\n\n",
    )?;
    let editor_result = run_editor(terminal, &path);
    if editor_result.is_err() {
        let _ = fs::remove_file(&path);
        return editor_result;
    }
    let trace = Trace::parse_file(&path);
    let _ = fs::remove_file(&path);
    let mut trace = trace?;
    if trace.frontmatter.title.is_empty() {
        return Ok(());
    }
    if !VALID_TYPES.contains(&trace.frontmatter.trace_type.as_str()) {
        trace.frontmatter.trace_type = "note".into();
    }
    let mut new_trace = Trace::new(
        trace.frontmatter.title,
        trace.frontmatter.trace_type,
        trace.frontmatter.author,
        trace.frontmatter.tags,
        trace.body,
    );
    model.cortex.add(&mut new_trace)?;
    model.status = format!("Added {}", new_trace.frontmatter.id);
    model.reload()
}

fn edit_existing(
    terminal: &mut Terminal<CrosstermBackend<io::Stdout>>,
    model: &mut Model<'_>,
    id: &str,
) -> Result<()> {
    let row = model.cortex.get(id)?;
    let path = model.cortex.file_path(&row);
    run_editor(terminal, &path)?;
    let mut trace = Trace::parse_file(&path)?;
    model.cortex.update_trace(id, &mut trace, false)?;
    model.status = format!("Updated {id}");
    model.reload()
}

fn run_editor(
    terminal: &mut Terminal<CrosstermBackend<io::Stdout>>,
    path: &std::path::Path,
) -> Result<()> {
    disable_raw_mode()?;
    execute!(terminal.backend_mut(), LeaveAlternateScreen)?;
    terminal.show_cursor()?;
    let editor = std::env::var("EDITOR")
        .or_else(|_| std::env::var("VISUAL"))
        .unwrap_or_else(|_| "vi".into());
    let status = Command::new(&editor)
        .arg(path)
        .status()
        .with_context(|| format!("starting editor {editor:?}"));
    enable_raw_mode()?;
    execute!(terminal.backend_mut(), EnterAlternateScreen)?;
    terminal.clear()?;
    let status = status?;
    if !status.success() {
        bail!("editor {editor:?} exited with {status}")
    }
    Ok(())
}

fn new_trace_path() -> PathBuf {
    std::env::temp_dir().join(format!("noema-new-{}.md", ulid::Ulid::new()))
}

fn resolve_theme(theme: &str) -> &'static str {
    match theme {
        "light" => "light",
        "dark" => "dark",
        _ => {
            let light = std::env::var("COLORFGBG")
                .ok()
                .and_then(|value| value.rsplit(';').next()?.parse::<u8>().ok())
                .is_some_and(|background| background >= 7);
            if light { "light" } else { "dark" }
        }
    }
}

fn tier_badge(tier: &str) -> &'static str {
    match tier {
        "short" => "s",
        "mid" => "m",
        "long" => "L",
        _ => "?",
    }
}

fn tier_style(tier: &str, dimmed: bool, palette: Palette) -> Style {
    if dimmed {
        return Style::default();
    }
    match tier {
        "short" => palette.tier_short,
        "mid" => palette.tier_mid,
        "long" => palette.tier_long,
        _ => Style::default(),
    }
}

fn list_title_width(area_width: u16) -> usize {
    // Right border, three inter-column spaces, and fixed cursor/tier,
    // type, and date columns consume 32 cells.
    usize::from(area_width.saturating_sub(32).max(4))
}

fn signed_count(value: i64) -> String {
    if value > 0 {
        format!("+{value}")
    } else {
        value.to_string()
    }
}

fn truncate(value: &str, maximum: usize) -> String {
    let characters = value.chars().collect::<Vec<_>>();
    if characters.len() <= maximum {
        return value.to_owned();
    }
    if maximum <= 1 {
        return "…".chars().take(maximum).collect();
    }
    let mut output = characters[..maximum - 1].iter().collect::<String>();
    output.push('…');
    output
}

fn wrap_line(value: &str, width: usize) -> Vec<String> {
    if width == 0 || value.chars().count() <= width {
        return vec![value.to_owned()];
    }
    let mut remaining = value.trim_end();
    let mut lines = Vec::new();
    while remaining.chars().count() > width {
        let prefix = remaining.chars().take(width).collect::<String>();
        let split = prefix.rfind(' ').unwrap_or(prefix.len());
        let split = if split == 0 { prefix.len() } else { split };
        lines.push(remaining[..split].to_owned());
        remaining = remaining[split..].trim_start();
    }
    if !remaining.is_empty() {
        lines.push(remaining.to_owned());
    }
    lines
}

fn metadata(label: &str, value: &str, palette: Palette) -> Line<'static> {
    Line::from(vec![
        Span::styled(format!("  {:<10}", format!("{label}:")), palette.dim),
        Span::styled(value.to_owned(), palette.surface),
    ])
}

fn metadata_chip(label: &str, value: &str, palette: Palette) -> Line<'static> {
    Line::from(vec![
        Span::styled(format!("  {:<10}", format!("{label}:")), palette.dim),
        Span::styled(format!(" #{value} "), palette.chip),
    ])
}

fn metadata_tag_chips(tags: &[String], value_width: usize, palette: Palette) -> Vec<Line<'static>> {
    let mut output = Vec::new();
    let mut spans = Vec::new();
    let mut width = 0;
    let mut first = true;
    for tag in tags {
        let chip = format!(" #{tag} ");
        let chip_width = chip.chars().count();
        let gap = usize::from(!spans.is_empty());
        if !spans.is_empty() && width + gap + chip_width > value_width {
            let label = if first { "tags:" } else { "" };
            let mut line = vec![Span::styled(format!("  {label:<10}"), palette.dim)];
            line.append(&mut spans);
            output.push(Line::from(line));
            spans = Vec::new();
            width = 0;
            first = false;
        }
        if !spans.is_empty() {
            spans.push(Span::styled(" ", palette.surface));
            width += 1;
        }
        spans.push(Span::styled(chip, palette.chip));
        width += chip_width;
    }
    if !spans.is_empty() {
        let label = if first { "tags:" } else { "" };
        let mut line = vec![Span::styled(format!("  {label:<10}"), palette.dim)];
        line.append(&mut spans);
        output.push(Line::from(line));
    }
    output
}

fn detail_separator(
    area_width: u16,
    scroll: usize,
    visible: usize,
    total: usize,
    palette: Palette,
) -> Line<'static> {
    let rule_width = usize::from(area_width.saturating_sub(4).max(1));
    if total <= visible {
        return Line::from(Span::styled(
            format!("  {}", "─".repeat(rule_width)),
            palette.divider,
        ));
    }
    let maximum = total.saturating_sub(visible);
    let glyph = if scroll > 0 && scroll < maximum {
        "▴▾"
    } else if scroll > 0 {
        "▴"
    } else {
        "▾"
    };
    let upper = (scroll + visible).min(total);
    let indicator = format!("{glyph} {upper}/{total}");
    if rule_width > indicator.chars().count() + 2 {
        let dashes = rule_width - indicator.chars().count() - 1;
        Line::from(vec![
            Span::styled(format!("  {} ", "─".repeat(dashes)), palette.divider),
            Span::styled(indicator, palette.dim),
        ])
    } else {
        Line::from(Span::styled(
            format!("  {}", "─".repeat(rule_width)),
            palette.divider,
        ))
    }
}

fn centered_rect(percent_x: u16, height: u16, area: Rect) -> Rect {
    let vertical = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Fill(1),
            Constraint::Length(height.min(area.height)),
            Constraint::Fill(1),
        ])
        .split(area);
    let horizontal = Layout::default()
        .direction(Direction::Horizontal)
        .constraints([
            Constraint::Percentage((100 - percent_x) / 2),
            Constraint::Percentage(percent_x),
            Constraint::Percentage((100 - percent_x) / 2),
        ])
        .split(vertical[1]);
    horizontal[1]
}

#[cfg(test)]
mod tests {
    use super::*;
    use ratatui::backend::TestBackend;

    fn key(code: KeyCode) -> KeyEvent {
        KeyEvent::new(code, KeyModifiers::NONE)
    }

    fn fixture() -> (tempfile::TempDir, Cortex) {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("tui-test", temp.path()).unwrap();
        let cortex = Cortex::open("tui-test", temp.path().join("tui-test")).unwrap();
        for (title, tier) in [("Alpha", "short"), ("Beta", "mid"), ("Gamma", "long")] {
            let mut trace = Trace::new(title, "fact", "tester", vec!["tui".into()], title);
            trace.frontmatter.tier = tier.into();
            cortex.add(&mut trace).unwrap();
        }
        (temp, cortex)
    }

    #[test]
    fn defaults_hide_long_and_navigation_tracks_focus() {
        let (_temp, cortex) = fixture();
        let mut model = Model::new(&cortex, "dark").unwrap();
        assert_eq!(model.rows.len(), 2);
        model.update(key(KeyCode::Char('j'))).unwrap();
        assert_eq!(model.cursor, 1);
        model.update(key(KeyCode::Right)).unwrap();
        assert_eq!(model.focus, FocusPane::Detail);
        model.update(key(KeyCode::Char('j'))).unwrap();
        assert_eq!(model.cursor, 1);
        assert_eq!(model.detail_scroll, 1);
        model.update(key(KeyCode::Esc)).unwrap();
        assert_eq!(model.focus, FocusPane::List);
    }

    #[test]
    fn tier_toggles_search_and_help_match_go_state_transitions() {
        let (_temp, cortex) = fixture();
        let mut model = Model::new(&cortex, "dark").unwrap();
        model.update(key(KeyCode::Char('3'))).unwrap();
        assert_eq!(model.rows.len(), 3);
        model.update(key(KeyCode::Char('/'))).unwrap();
        assert_eq!(model.mode, Mode::Search);
        for character in "Alpha".chars() {
            model.update(key(KeyCode::Char(character))).unwrap();
        }
        model.update(key(KeyCode::Enter)).unwrap();
        assert_eq!(model.rows.len(), 1);
        assert_eq!(model.query, "Alpha");
        model.update(key(KeyCode::Esc)).unwrap();
        assert!(model.query.is_empty());
        model.update(key(KeyCode::Char('?'))).unwrap();
        assert_eq!(model.mode, Mode::Help);
        model.update(key(KeyCode::Esc)).unwrap();
        assert_eq!(model.mode, Mode::List);
    }

    #[test]
    fn refresh_preserves_selection_and_new_row_highlight_fades() {
        let (_temp, cortex) = fixture();
        let mut model = Model::new(&cortex, "dark").unwrap();
        model.cursor = model.rows.len() - 1;
        let selected = model.selected_id().unwrap().to_owned();
        let mut trace = Trace::new("Delta", "fact", "tester", vec![], "new row");
        let added = trace.frontmatter.id.clone();
        cortex.add(&mut trace).unwrap();

        model.reload().unwrap();
        assert_eq!(model.selected_id(), Some(selected.as_str()));
        assert_eq!(
            model.new_row_ttl.get(&added),
            Some(&NEW_ROW_HIGHLIGHT_TICKS)
        );
        model.reload().unwrap();
        assert_eq!(model.new_row_ttl.get(&added), Some(&1));
        model.reload().unwrap();
        assert!(!model.new_row_ttl.contains_key(&added));

        model.visible_long = true;
        model.reload().unwrap();
        assert!(model.new_row_ttl.is_empty());
    }

    #[test]
    fn vote_cycle_archive_trash_recover_and_purge_mutate_once() {
        let (_temp, cortex) = fixture();
        let mut model = Model::new(&cortex, "dark").unwrap();
        let id = model.selected_id().unwrap().to_owned();
        model.update(key(KeyCode::Char('+'))).unwrap();
        assert_eq!(cortex.tier_votes(&id).unwrap(), 1);
        model.update(key(KeyCode::Char('+'))).unwrap();
        assert_eq!(cortex.tier_votes(&id).unwrap(), 0);

        model.update(key(KeyCode::Char('d'))).unwrap();
        assert_eq!(model.mode, Mode::Confirm);
        model.update(key(KeyCode::Char('y'))).unwrap();
        assert!(!cortex.get(&id).unwrap().archived_at.is_empty());
        model.show_all = true;
        model.reload().unwrap();
        model.cursor = model.rows.iter().position(|row| row.id == id).unwrap();
        model.update(key(KeyCode::Char('D'))).unwrap();
        model.update(key(KeyCode::Enter)).unwrap();
        assert!(!cortex.get(&id).unwrap().trashed_at.is_empty());

        model.update(key(KeyCode::Char('t'))).unwrap();
        model.cursor = model.rows.iter().position(|row| row.id == id).unwrap();
        model.update(key(KeyCode::Char('r'))).unwrap();
        assert!(cortex.get(&id).unwrap().trashed_at.is_empty());

        model.update(key(KeyCode::Char('t'))).unwrap();
        model.cursor = model.rows.iter().position(|row| row.id == id).unwrap();
        model.update(key(KeyCode::Char('D'))).unwrap();
        model.update(key(KeyCode::Char('y'))).unwrap();
        model.update(key(KeyCode::Char('t'))).unwrap();
        model.cursor = model.rows.iter().position(|row| row.id == id).unwrap();
        model.update(key(KeyCode::Char('D'))).unwrap();
        model.update(key(KeyCode::Enter)).unwrap();
        assert!(cortex.get(&id).is_err());
    }

    #[test]
    fn renderer_surfaces_brand_rows_detail_filters_and_help() {
        let (_temp, cortex) = fixture();
        let mut model = Model::new(&cortex, "dark").unwrap();
        let mut terminal = Terminal::new(TestBackend::new(120, 28)).unwrap();
        terminal.draw(|frame| model.render(frame)).unwrap();
        let screen = buffer_text(terminal.backend());
        for expected in [
            "Noema.",
            "tui-test",
            "2 traces",
            "Alpha",
            "[fact]",
            "tier",
            "1/2/3:tier",
        ] {
            assert!(screen.contains(expected), "missing {expected:?}:\n{screen}");
        }

        model.update(key(KeyCode::Char('?'))).unwrap();
        terminal.draw(|frame| model.render(frame)).unwrap();
        let help = buffer_text(terminal.backend());
        assert!(help.contains("Navigation"));
        assert!(help.contains("Trace actions"));
        assert!(help.contains("Views & filters"));
    }

    #[test]
    fn renderer_matches_go_visual_hierarchy_at_fixed_size() {
        let (_temp, cortex) = fixture();
        let long_title = "A deliberately long title that must show truncation";
        let mut trace = Trace::new(
            long_title,
            "observation",
            "tester",
            vec!["visual-contract".into(), "second-tag".into()],
            "body",
        );
        cortex.add(&mut trace).unwrap();
        let model = Model::new(&cortex, "dark").unwrap();
        let mut terminal = Terminal::new(TestBackend::new(120, 28)).unwrap();
        terminal.draw(|frame| model.render(frame)).unwrap();
        let screen = buffer_text(terminal.backend());
        let rows = screen.lines().collect::<Vec<_>>();

        assert_eq!(rows[0].rfind("3 traces"), Some(112));
        assert!(screen.contains("id:"));
        assert!(screen.contains("type:"));
        assert!(screen.contains("#observation"));
        assert!(screen.contains("#visual-contract"));

        let body = Rect::new(0, 1, 120, 26);
        let panes = Layout::default()
            .direction(Direction::Horizontal)
            .constraints([
                Constraint::Percentage(LIST_PERCENT),
                Constraint::Percentage(100 - LIST_PERCENT),
            ])
            .split(body);
        let expected_title = truncate(long_title, list_title_width(panes[0].width));
        assert!(expected_title.ends_with('…'));
        assert!(screen.contains(&expected_title));

        let (chip_x, chip_y) = find_text(terminal.backend(), "#observation").unwrap();
        let chip_cell = terminal.backend().buffer().cell((chip_x, chip_y)).unwrap();
        assert_eq!(chip_cell.bg, Color::Rgb(236, 228, 212));
        assert_eq!(chip_cell.fg, Color::Rgb(26, 26, 26));

        let mut saw_short = false;
        let mut saw_mid = false;
        for y in 1..4 {
            let cell = terminal.backend().buffer().cell((2, y)).unwrap();
            match cell.symbol() {
                "s" => {
                    assert_eq!(cell.fg, Color::Indexed(208));
                    saw_short = true;
                }
                "m" => {
                    assert_eq!(cell.fg, Color::Indexed(221));
                    saw_mid = true;
                }
                _ => {}
            }
        }
        assert!(saw_short && saw_mid);

        let separator = rows
            .iter()
            .find(|row| row.contains('─'))
            .expect("detail separator");
        assert_eq!(
            separator
                .chars()
                .filter(|character| *character == '─')
                .count(),
            usize::from(panes[1].width.saturating_sub(4))
        );

        let light_model = Model::new(&cortex, "light").unwrap();
        let mut light_terminal = Terminal::new(TestBackend::new(120, 28)).unwrap();
        light_terminal
            .draw(|frame| light_model.render(frame))
            .unwrap();
        let (chip_x, chip_y) = find_text(light_terminal.backend(), "#observation").unwrap();
        let chip_cell = light_terminal
            .backend()
            .buffer()
            .cell((chip_x, chip_y))
            .unwrap();
        assert_eq!(chip_cell.bg, Color::Rgb(26, 26, 26));
        assert_eq!(chip_cell.fg, Color::Rgb(255, 255, 255));
        let mut saw_short = false;
        let mut saw_mid = false;
        for y in 1..4 {
            let cell = light_terminal.backend().buffer().cell((2, y)).unwrap();
            match cell.symbol() {
                "s" => {
                    assert_eq!(cell.fg, Color::Indexed(166));
                    saw_short = true;
                }
                "m" => {
                    assert_eq!(cell.fg, Color::Indexed(136));
                    saw_mid = true;
                }
                _ => {}
            }
        }
        assert!(saw_short && saw_mid);
    }

    #[test]
    fn detail_scroll_keeps_metadata_pinned_and_reports_position() {
        let (_temp, cortex) = fixture();
        let mut model = Model::new(&cortex, "dark").unwrap();
        let id = model.selected_id().unwrap().to_owned();
        let (_, mut trace) = cortex.get_trace(&id).unwrap();
        trace.body = (0..20)
            .map(|index| format!("line {index:02}"))
            .collect::<Vec<_>>()
            .join("\n");
        cortex.update_trace(&id, &mut trace, false).unwrap();
        model.reload().unwrap();
        model.focus = FocusPane::Detail;
        model.detail_scroll = usize::MAX;

        let mut terminal = Terminal::new(TestBackend::new(80, 14)).unwrap();
        terminal.draw(|frame| model.render(frame)).unwrap();
        let screen = buffer_text(terminal.backend());
        assert!(screen.contains("id:"));
        assert!(screen.contains("created:"));
        assert!(screen.contains("▴ 20/20"));
        assert!(screen.contains("line 19"));
        assert!(!screen.contains("line 00"));
    }

    #[test]
    fn helpers_preserve_tier_vote_and_theme_contracts() {
        assert_eq!(tier_badge("short"), "s");
        assert_eq!(tier_badge("mid"), "m");
        assert_eq!(tier_badge("long"), "L");
        assert_eq!(tier_badge(""), "?");
        assert_eq!(signed_count(2), "+2");
        assert_eq!(signed_count(0), "0");
        assert_eq!(signed_count(-2), "-2");
        assert_eq!(resolve_theme("dark"), "dark");
        assert_eq!(resolve_theme("light"), "light");
        assert_eq!(truncate("abcdef", 4), "abc…");
        assert_eq!(truncate("éclair", 3), "éc…");
        assert_eq!(wrap_line("alpha beta gamma", 10), ["alpha", "beta gamma"]);
    }

    fn buffer_text(backend: &TestBackend) -> String {
        let width = backend.buffer().area().width as usize;
        backend
            .buffer()
            .content()
            .chunks(width)
            .map(|row| row.iter().map(|cell| cell.symbol()).collect::<String>())
            .collect::<Vec<_>>()
            .join("\n")
    }

    fn find_text(backend: &TestBackend, needle: &str) -> Option<(u16, u16)> {
        let width = backend.buffer().area().width;
        for y in 0..backend.buffer().area().height {
            let row = (0..width)
                .map(|x| backend.buffer().cell((x, y)).unwrap().symbol())
                .collect::<String>();
            if let Some(x) = row.find(needle) {
                return Some((x as u16, y));
            }
        }
        None
    }
}
