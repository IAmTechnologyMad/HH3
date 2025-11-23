# Funcorp Hot Wheels Scraper Bot

A Telegram bot written in Go that monitors the [Funcorp Hot Wheels Collection](https://www.funcorp.in/collections/hot-wheels) for new items and restocks.

## Features

- **Automated Scraping:** Checks the website every 60 seconds.
- **New Item Alerts:** Sends a Telegram notification with the product title, price, and link when a new item is found.
- **Stock Tracking:** Ignores "Sold out" items until they come back in stock.
- **Commands:**
  - `/status`: Check if the bot is running.
  - `/dump`: Send a list of ALL currently in-stock items.
  - `/recent`: Show items found in the last 10 checks.
  - `/pause` / `/start`: Control the scraper.

## Setup & Run

1.  **Install Go:** Ensure you have Go installed on your machine.
2.  **Clone the repo:**
    ```bash
    git clone <your-repo-url>
    cd <your-repo-folder>
    ```
3.  **Run the bot:**
    ```bash
    go run main.go
    ```

## Configuration

The configuration (Telegram Token, Chat IDs) is currently hardcoded in `main.go`.

## Deployment

This bot includes a simple HTTP server and a keep-alive mechanism, making it suitable for deployment on free tiers of services like **Render** or **Heroku**.

- **Build Command:** `go build -o main .`
- **Start Command:** `./main`
