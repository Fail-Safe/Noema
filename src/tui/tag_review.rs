use std::sync::mpsc::{self, Receiver};

use super::*;
use crate::cortex::tag_review::TagReview;
use crate::tag_review::{ModelMode, ResolvedModel, TagReviewConfig};

struct SettingsEditor {
    original: TagReviewConfig,
    draft: TagReviewConfig,
    shared_defaults: Option<crate::cortex::ConsolidationConfig>,
    field: usize,
}

#[derive(Default)]
pub(super) struct ReviewState {
    items: Vec<TagReview>,
    cursor: usize,
    scroll: u16,
    editing: Option<(String, String)>,
    confirm: Option<(char, Option<String>)>,
    worker: Option<Receiver<std::result::Result<String, String>>>,
    offset: usize,
    model_label: String,
    confirm_model: Option<ResolvedModel>,
    running_model: Option<String>,
    settings: Option<SettingsEditor>,
}

impl Model<'_> {
    pub(super) fn reload_tag_reviews(&mut self) -> Result<()> {
        self.review.model_label =
            crate::tag_review::model_status(&crate::cortex::read_manifest(&self.cortex.dir)?);
        let selected = self
            .review
            .items
            .get(self.review.cursor)
            .map(|p| p.id.clone());
        self.review.items = self.cortex.tag_reviews()?;
        let missing_confirmation = self
            .review
            .confirm
            .as_ref()
            .and_then(|(_, id)| id.as_ref())
            .is_some_and(|id| !self.review.items.iter().any(|p| &p.id == id));
        let missing_edit = self
            .review
            .editing
            .as_ref()
            .is_some_and(|(id, _)| !self.review.items.iter().any(|p| &p.id == id));
        if missing_confirmation {
            self.review.confirm = None;
            self.review.confirm_model = None;
        }
        if missing_edit {
            self.review.editing = None;
        }
        if missing_confirmation || missing_edit {
            self.status = "Selected proposal no longer exists; action cancelled.".into();
        }
        if let Some(index) =
            selected.and_then(|id| self.review.items.iter().position(|p| p.id == id))
        {
            self.review.cursor = index;
        }
        self.review.cursor = self
            .review
            .cursor
            .min(self.review.items.len().saturating_sub(1));
        Ok(())
    }

    pub(super) fn poll_tag_review(&mut self) {
        let result = self
            .review
            .worker
            .as_ref()
            .and_then(|worker| match worker.try_recv() {
                Ok(result) => Some(result),
                Err(mpsc::TryRecvError::Disconnected) => {
                    Some(Err("Tag Review worker stopped unexpectedly".into()))
                }
                Err(mpsc::TryRecvError::Empty) => None,
            });
        if let Some(result) = result {
            self.review.worker = None;
            self.review.running_model = None;
            match result {
                Ok(message) => self.status = message,
                Err(error) => self.error = error,
            }
            if let Err(error) = self.reload_tag_reviews() {
                self.error = error.to_string();
            }
        }
    }

    fn start_tag_analysis(&mut self) -> Result<()> {
        if self.review.worker.is_some() {
            bail!("Tag Review analysis is already running");
        }
        let config = self
            .review
            .confirm_model
            .take()
            .context("select a semantic model before confirming analysis")?;
        self.review.running_model =
            Some(format!("{} · {}", config.mode.label(), config.model_name));
        let (sender, receiver) = mpsc::channel();
        let name = self.cortex.name.clone();
        let dir = self.cortex.dir.clone();
        let offset = self.review.offset;
        std::thread::spawn(move || {
            let result = (|| -> Result<String> {
                let cx = Cortex::open(name, dir)?;
                let runtime = tokio::runtime::Builder::new_current_thread().enable_all().build()?;
                let result = runtime.block_on(crate::tag_review::analyze_with_model(cx, offset, config))?;
                if let Some(warning) = &result.warning {
                    bail!("{warning}; analyzed {}/{} requested traces, queued {} (retained), unfinished {}. No tags changed. Retry this window with m.",
                        result.analyzed, result.requested, result.queued, result.unfinished);
                }
                Ok(format!("{} ({}): analyzed {} of {} eligible traces (offset {}); queued {}, skipped {}, split retries {}. No tags changed.",
                    result.model_name, result.model_mode.label(), result.analyzed, result.eligible, offset, result.queued, result.skipped, result.split_retries))
            })().map_err(|error| error.to_string());
            let _ = sender.send(result);
        });
        self.review.worker = Some(receiver);
        self.status = "Model analysis running; you can keep browsing.".into();
        Ok(())
    }

    pub(super) fn update_tag_review(&mut self, key: KeyEvent) -> Result<InputAction> {
        if key.modifiers.contains(KeyModifiers::CONTROL) && key.code == KeyCode::Char('c') {
            return Ok(InputAction::Quit);
        }
        if self.review.settings.is_some() {
            return self.update_tag_review_settings(key);
        }
        if let Some((id, input)) = self.review.editing.as_mut() {
            match key.code {
                KeyCode::Esc => self.review.editing = None,
                KeyCode::Enter => {
                    let tags: Vec<String> =
                        serde_json::from_str(input).context("enter tags as a JSON array")?;
                    self.cortex.edit_tag_review(id, tags)?;
                    self.review.editing = None;
                    self.reload_tag_reviews()?;
                    self.status = "Edited proposal queued; review it before applying.".into();
                }
                KeyCode::Backspace => {
                    input.pop();
                }
                KeyCode::Char(c)
                    if !key
                        .modifiers
                        .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT) =>
                {
                    if input.len() < 16384 {
                        input.push(c);
                    }
                }
                _ => {}
            }
            return Ok(InputAction::Continue);
        }
        if let Some((action, id)) = self.review.confirm.take() {
            if key.code == KeyCode::Char('y') {
                if action == 'm' {
                    self.start_tag_analysis()?;
                } else if let Some(id) = id {
                    self.cortex.apply_tag_review(&id, action == 'u')?;
                    self.reload_tag_reviews()?;
                    self.reload()?;
                    self.status = if action == 'u' {
                        "Tag changes undone."
                    } else {
                        "Approved tag changes applied."
                    }
                    .into();
                }
            }
            self.review.confirm_model = None;
            return Ok(InputAction::Continue);
        }
        match key.code {
            KeyCode::Char('q') => return Ok(InputAction::Quit),
            KeyCode::Esc | KeyCode::Char('T') => {
                self.mode = Mode::List;
                self.reload()?;
            }
            KeyCode::Down | KeyCode::Char('j') => {
                self.review.cursor =
                    (self.review.cursor + 1).min(self.review.items.len().saturating_sub(1));
                self.review.scroll = 0;
            }
            KeyCode::Up | KeyCode::Char('k') => {
                self.review.cursor = self.review.cursor.saturating_sub(1);
                self.review.scroll = 0;
            }
            KeyCode::PageDown => self.review.scroll = self.review.scroll.saturating_add(10),
            KeyCode::PageUp => self.review.scroll = self.review.scroll.saturating_sub(10),
            KeyCode::Char('R') => self.reload_tag_reviews()?,
            KeyCode::Char('h') => {
                let added = self.cortex.review_tag_hygiene()?;
                self.reload_tag_reviews()?;
                self.status = format!("Queued {added} new hygiene suggestions. No tags changed.");
            }
            KeyCode::Char('m') => {
                if self.review.worker.is_some() {
                    bail!("model analysis is already running");
                }
                let manifest = crate::cortex::read_manifest(&self.cortex.dir)?;
                let model = crate::tag_review::resolve_model(&manifest)?;
                self.review.model_label = format!("{} · {}", model.mode.label(), model.model_name);
                self.review.confirm_model = Some(model);
                self.review.confirm = Some(('m', None));
            }
            KeyCode::Char('s') => {
                let manifest = crate::cortex::read_manifest(&self.cortex.dir)?;
                let original = manifest.tag_review.clone().unwrap_or_default();
                // Do not display a potentially credential-bearing malformed URL.
                let mut checked = original.clone();
                checked.mode = ModelMode::Shared;
                checked.validate()?;
                self.review.settings = Some(SettingsEditor {
                    draft: original.clone(),
                    original,
                    shared_defaults: manifest.consolidation_config().ok().flatten(),
                    field: 0,
                });
                self.review.scroll = 0;
            }
            KeyCode::Char('[') => self.review.offset = self.review.offset.saturating_sub(40),
            KeyCode::Char(']') => self.review.offset = self.review.offset.saturating_add(40),
            KeyCode::Char('a') | KeyCode::Char('u') if !self.review.items.is_empty() => {
                self.review.confirm = match key.code {
                    KeyCode::Char(c) => self
                        .review
                        .items
                        .get(self.review.cursor)
                        .map(|p| (c, Some(p.id.clone()))),
                    _ => None,
                };
            }
            KeyCode::Char('x') | KeyCode::Char('d') | KeyCode::Char('p') => {
                if let Some(proposal) = self.review.items.get(self.review.cursor) {
                    let status = match key.code {
                        KeyCode::Char('x') => "rejected",
                        KeyCode::Char('d') => "deferred",
                        _ => "pending",
                    };
                    self.cortex.set_tag_review_status(&proposal.id, status)?;
                    self.reload_tag_reviews()?;
                    self.status = format!("Review {status}.");
                }
            }
            KeyCode::Char('e') => {
                if let Some(proposal) = self.review.items.get(self.review.cursor) {
                    self.review.editing =
                        Some((proposal.id.clone(), serde_json::to_string(&proposal.after)?));
                }
            }
            _ => {}
        }
        Ok(InputAction::Continue)
    }

    pub(super) fn tag_review_footer(&self, palette: Palette) -> (String, Style) {
        if !self.error.is_empty() {
            return (self.error.clone(), palette.error);
        }
        if self.review.settings.is_some() {
            return (
                "tab/↑/↓:field  space/←/→:mode  ctrl+u:clear  ctrl+s:save  esc:cancel".into(),
                palette.dim,
            );
        }
        if self.review.editing.is_some() {
            return (
                "Edit desired tags above: JSON array · enter:save proposal · esc:cancel".into(),
                palette.dim,
            );
        }
        if let Some((action, _)) = &self.review.confirm {
            if *action == 'm' {
                let label = self
                    .review
                    .confirm_model
                    .as_ref()
                    .map(|m| format!("{} ({})", m.model_name, m.mode.label()))
                    .unwrap_or_default();
                return (
                    format!(
                        "Send up to 40 trace excerpts to {label}? y:confirm · any other key:cancel"
                    ),
                    palette.error,
                );
            }
            let text = match *action {
                'u' => "Undo the selected applied change? y:confirm · any other key:cancel",
                _ => {
                    "Apply the exact selected before → after tag change? y:approve · any other key:cancel"
                }
            };
            return (text.into(), palette.error);
        }
        if !self.status.is_empty() {
            return (self.status.clone(), palette.success);
        }
        ("j/k:nav  h:hygiene  m:model  s:settings  a:approve  e:edit  x:reject  d:defer  p:resume  u:undo  R:refresh  esc:traces".into(), palette.dim)
    }

    pub(super) fn render_tag_review(&self, frame: &mut Frame, area: Rect, palette: Palette) {
        let (list, detail) = pane_areas(area);
        let visible = usize::from(list.height.saturating_sub(2));
        let start = self.review.cursor.saturating_sub(visible.saturating_sub(1));
        let lines: Vec<Line<'static>> = self
            .review
            .items
            .iter()
            .enumerate()
            .skip(start)
            .take(visible)
            .map(|(i, p)| {
                Line::from(Span::styled(
                    format!(" [{}] {}", p.status, p.title),
                    if i == self.review.cursor {
                        palette.selected
                    } else {
                        palette.surface
                    },
                ))
            })
            .collect();
        frame.render_widget(
            Paragraph::new(lines)
                .block(Block::default().borders(Borders::ALL).title(format!(
                    "Tag Review · {} proposals",
                    self.review.items.len()
                )))
                .style(palette.surface),
            list,
        );
        let mut text = format!(
            "AI-assisted taxonomy maintenance\nSemantic model: {}\ns: model settings (shared/separate)\nOn demand only; no automatic changes.\nh: hygiene (local) · m: model (requires consent)\n[/]: model batch offset (currently {})\nPgUp/PgDn: scroll preview\n\n",
            self.review.model_label, self.review.offset
        );
        if let Some(model) = &self.review.running_model {
            text.push_str(&format!("Model analysis running: {model}…\n\n"));
        }
        if let Some(proposal) = self.review.items.get(self.review.cursor) {
            text.push_str(&format!("{}\n{}\nStatus: {} · Source: {}\nAffected traces: 1\n\nBEFORE\n{}\n\nAFTER\n{}\n\nRATIONALE\n{}\n\nSUPPORTING TRACE EXCERPT (may be truncated)\n{}\n",
                proposal.title, proposal.trace_id, proposal.status, proposal.source,
                serde_json::to_string(&proposal.before).unwrap_or_default(),
                serde_json::to_string(&proposal.after).unwrap_or_default(), proposal.rationale, proposal.evidence));
        } else {
            text.push_str(
                "No suggestions yet. Press h to check hygiene or m to request a model review.",
            );
        }
        if let Some((_, input)) = &self.review.editing {
            text = format!(
                "EDIT DESIRED TAGS (JSON array)\n\n{input}\n\nBackspace edits; enter queues the revised proposal. Esc cancels.\nNo trace tags change until approval."
            );
        }
        if let Some(editor) = &self.review.settings {
            text = format!(
                "SEMANTIC TAG REVIEW SETTINGS\n\nShared uses consolidation's model, endpoint, and credential reference.\nSeparate uses only the fields below; consolidation is unchanged.\n\n{}\n\nOnly environment variable names are stored, never API key values.\nSwitching back to Shared preserves saved separate settings.\nNo model is called when settings are saved.\n\nText editing: type / backspace; ctrl+u clears the selected field.\nTab or ↑/↓ selects a field; space or ←/→ toggles mode.\nCtrl+s saves; Esc cancels.",
                editor.lines()
            );
        }
        frame.render_widget(
            Paragraph::new(text)
                .wrap(ratatui::widgets::Wrap { trim: false })
                .scroll((self.review.scroll, 0))
                .block(
                    Block::default()
                        .borders(Borders::ALL)
                        .title("Preview & evidence"),
                )
                .style(palette.surface),
            detail,
        );
    }

    fn update_tag_review_settings(&mut self, key: KeyEvent) -> Result<InputAction> {
        if key.code == KeyCode::Esc {
            self.review.settings = None;
            return Ok(InputAction::Continue);
        }
        let editor = self
            .review
            .settings
            .as_mut()
            .expect("settings editor is open");
        if key.modifiers.contains(KeyModifiers::CONTROL) && key.code == KeyCode::Char('s') {
            self.cortex
                .save_tag_review_settings(&editor.draft, Some(&editor.original))?;
            self.review.settings = None;
            self.reload_tag_reviews()?;
            self.status = "Tag Review settings saved; consolidation unchanged.".into();
            return Ok(InputAction::Continue);
        }
        match key.code {
            KeyCode::Tab | KeyCode::Down => editor.field = (editor.field + 1) % 4,
            KeyCode::BackTab | KeyCode::Up => editor.field = (editor.field + 3) % 4,
            KeyCode::Left | KeyCode::Right | KeyCode::Char(' ') if editor.field == 0 => {
                editor.toggle()
            }
            KeyCode::Char('u') if key.modifiers.contains(KeyModifiers::CONTROL) => {
                if let Some(value) = editor.selected_text() {
                    value.clear();
                }
            }
            KeyCode::Backspace => {
                if let Some(value) = editor.selected_text() {
                    value.pop();
                }
            }
            KeyCode::Char(c)
                if !key
                    .modifiers
                    .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT)
                    && !c.is_control() =>
            {
                if let Some(value) = editor.selected_text()
                    && value.len() < 2048
                {
                    value.push(c);
                }
            }
            _ => {}
        }
        Ok(InputAction::Continue)
    }
}

impl SettingsEditor {
    fn selected_text(&mut self) -> Option<&mut String> {
        match self.field {
            1 => Some(&mut self.draft.model_name),
            2 => Some(&mut self.draft.endpoint),
            3 => Some(&mut self.draft.api_key_env),
            _ => None,
        }
    }

    fn toggle(&mut self) {
        self.draft.mode = match self.draft.mode {
            ModelMode::Shared => ModelMode::Separate,
            ModelMode::Separate => ModelMode::Shared,
        };
        if self.draft.mode == ModelMode::Separate
            && self.draft.model_name.is_empty()
            && self.draft.endpoint.is_empty()
            && let Some(shared) = &self.shared_defaults
        {
            let candidate = TagReviewConfig {
                mode: ModelMode::Separate,
                model_name: shared.model_name.clone(),
                endpoint: shared.local_llm_endpoint.clone(),
                api_key_env: shared.api_key_env.clone(),
            };
            // Never copy/display a credential-bearing or malformed shared URL.
            if candidate.validate().is_ok() {
                self.draft = candidate;
            }
        }
    }

    fn lines(&self) -> String {
        [
            format!("Mode: {}", self.draft.mode.label()),
            format!("Separate model: {}", self.draft.model_name),
            format!("Separate endpoint: {}", self.draft.endpoint),
            format!(
                "Separate API key env (optional): {}",
                self.draft.api_key_env
            ),
        ]
        .into_iter()
        .enumerate()
        .map(|(i, value)| format!("{} {value}", if i == self.field { ">" } else { " " }))
        .collect::<Vec<_>>()
        .join("\n\n")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key(code: KeyCode) -> KeyEvent {
        KeyEvent::new(code, KeyModifiers::NONE)
    }
    fn ctrl(c: char) -> KeyEvent {
        KeyEvent::new(KeyCode::Char(c), KeyModifiers::CONTROL)
    }

    #[test]
    fn worker_refresh_cancels_missing_proposal_confirmation_and_edit() {
        for action in ['a', 'u', 'e'] {
            let temp = tempfile::tempdir().unwrap();
            Cortex::create("test", temp.path()).unwrap();
            let cx = Cortex::open("test", temp.path().join("test")).unwrap();
            let mut proposals = Vec::new();
            for title in ["Selected evidence", "Other evidence"] {
                let mut trace = crate::trace::Trace::new(
                    title,
                    "note",
                    "",
                    vec!["rust".into()],
                    "Concurrency notes",
                );
                cx.add(&mut trace).unwrap();
                proposals.push(
                    cx.propose_tag_review(
                        &trace.frontmatter.id,
                        vec!["rust".into(), "concurrency".into()],
                        "Evidence",
                        "fixture",
                    )
                    .unwrap()
                    .unwrap(),
                );
            }
            if action == 'u' {
                cx.apply_tag_review(&proposals[0].id, false).unwrap();
            }
            let mut model = Model::new(&cx, "dark").unwrap();
            model.update(key(KeyCode::Char('T'))).unwrap();
            model.review.cursor = model
                .review
                .items
                .iter()
                .position(|p| p.id == proposals[0].id)
                .unwrap();
            model.update(key(KeyCode::Char(action))).unwrap();
            cx.remove_hard(&proposals[0].trace_id).unwrap();
            let (sender, receiver) = mpsc::channel();
            model.review.worker = Some(receiver);
            sender.send(Ok("analysis completed".into())).unwrap();
            model.poll_tag_review();
            assert!(model.review.confirm.is_none());
            assert!(model.review.editing.is_none());
            model.update(key(KeyCode::Char('y'))).unwrap();
            model.update(key(KeyCode::Enter)).unwrap();
            assert_eq!(cx.tag_review(&proposals[1].id).unwrap().status, "pending");
            assert_eq!(cx.get(&proposals[1].trace_id).unwrap().tags, ["rust"]);
        }
    }

    #[test]
    fn settings_toggle_persists_separate_model_without_changing_consolidation() {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("test", temp.path()).unwrap();
        let mut cx = Cortex::open("test", temp.path().join("test")).unwrap();
        cx.manifest.consolidation = Some(serde_yaml::from_str("llm_enabled: true\nlocal_llm_endpoint: http://127.0.0.1:8000/v1\nmodel_name: fixture-9b\n").unwrap());
        crate::cortex::write_manifest(&cx.dir, &cx.manifest).unwrap();
        let mut model = Model::new(&cx, "dark").unwrap();
        model.update(key(KeyCode::Char('T'))).unwrap();
        assert!(model.review.model_label.contains("fixture-9b"));
        model.update(key(KeyCode::Char('s'))).unwrap();
        model.update(key(KeyCode::Char(' '))).unwrap();
        assert_eq!(
            model.review.settings.as_ref().unwrap().draft.mode,
            ModelMode::Separate
        );
        model.update(key(KeyCode::Tab)).unwrap();
        model.update(ctrl('u')).unwrap();
        for c in "fixture-35b".chars() {
            model.update(key(KeyCode::Char(c))).unwrap();
        }
        let mut terminal = Terminal::new(ratatui::backend::TestBackend::new(120, 40)).unwrap();
        terminal.draw(|frame| model.render(frame)).unwrap();
        model.update(ctrl('s')).unwrap();
        assert!(model.review.settings.is_none());
        assert!(model.review.model_label.contains("separate · fixture-35b"));
        let saved = crate::cortex::read_manifest(&cx.dir).unwrap();
        assert_eq!(saved.consolidation, cx.manifest.consolidation);
        assert_eq!(saved.tag_review.as_ref().unwrap().model_name, "fixture-35b");

        // Consent pins the exact model, even if another client edits settings.
        model.update(key(KeyCode::Char('m'))).unwrap();
        let mut shared = saved.tag_review.unwrap();
        shared.mode = ModelMode::Shared;
        cx.save_tag_review_settings(&shared, None).unwrap();
        assert_eq!(
            model.review.confirm_model.as_ref().unwrap().model_name,
            "fixture-35b"
        );
        model.update(key(KeyCode::Esc)).unwrap();
        assert!(model.review.confirm_model.is_none());
        assert!(model.review.worker.is_none());

        model.update(key(KeyCode::Char('R'))).unwrap();
        assert!(model.review.model_label.contains("shared · fixture-9b"));
        model.update(key(KeyCode::Char('s'))).unwrap();
        model.update(key(KeyCode::Char(' '))).unwrap();
        assert_eq!(
            model.review.settings.as_ref().unwrap().draft.model_name,
            "fixture-35b"
        );
        model.update(ctrl('s')).unwrap();
        assert!(model.review.model_label.contains("fixture-35b"));

        // Escape discards an uncommitted toggle.
        model.update(key(KeyCode::Char('s'))).unwrap();
        model.update(key(KeyCode::Char(' '))).unwrap();
        model.update(key(KeyCode::Esc)).unwrap();
        assert_eq!(cx.tag_review_settings().unwrap().mode, ModelMode::Separate);
    }

    #[test]
    fn invalid_shared_destination_is_not_copied_into_the_form() {
        let mut editor = SettingsEditor {
            original: TagReviewConfig::default(),
            draft: TagReviewConfig::default(),
            field: 0,
            shared_defaults: Some(crate::cortex::ConsolidationConfig {
                model_name: "fixture".into(),
                local_llm_endpoint: "https://user:synthetic-secret@example.com/v1".into(),
                ..Default::default()
            }),
        };
        editor.toggle();
        assert_eq!(editor.draft.mode, ModelMode::Separate);
        assert!(editor.draft.endpoint.is_empty());
        assert!(!editor.lines().contains("synthetic-secret"));
    }
}
