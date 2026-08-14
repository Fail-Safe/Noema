use anyhow::Result;
use crossterm::{
    event::{self, Event, KeyCode},
    execute,
    terminal::{EnterAlternateScreen, LeaveAlternateScreen, disable_raw_mode, enable_raw_mode},
};
use ratatui::{
    Terminal,
    backend::CrosstermBackend,
    widgets::{Block, Borders, Paragraph},
};

use crate::cortex::{Cortex, ListOptions};

pub fn run(cortex: &Cortex) -> Result<()> {
    enable_raw_mode()?;
    let mut stdout = std::io::stdout();
    execute!(stdout, EnterAlternateScreen)?;
    let mut terminal = Terminal::new(CrosstermBackend::new(stdout))?;
    let rows = cortex.list(&ListOptions::default())?;
    loop {
        terminal.draw(|frame| {
            let body = rows
                .iter()
                .map(|row| format!("{}  {}", row.id, row.title))
                .collect::<Vec<_>>()
                .join("\n");
            frame.render_widget(
                Paragraph::new(body)
                    .block(Block::default().title("Noema Rust").borders(Borders::ALL)),
                frame.area(),
            );
        })?;
        if let Event::Key(key) = event::read()?
            && matches!(key.code, KeyCode::Char('q') | KeyCode::Esc)
        {
            break;
        }
    }
    disable_raw_mode()?;
    execute!(terminal.backend_mut(), LeaveAlternateScreen)?;
    Ok(())
}
