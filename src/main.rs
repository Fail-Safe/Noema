#[tokio::main]
async fn main() {
    if let Err(error) = noema::cli::run().await {
        eprintln!("Error: {error:#}");
        std::process::exit(1);
    }
}
