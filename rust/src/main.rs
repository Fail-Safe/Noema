#[tokio::main]
async fn main() {
    if let Err(error) = noema_rs::cli::run().await {
        eprintln!("Error: {error:#}");
        std::process::exit(1);
    }
}
