# Telegram to Immich Bot

A lightweight Telegram bot written in Go that automatically uploads photos, videos, and documents from your Telegram chats to your self-hosted [Immich](https://immich.app/) instance.

## ✨ Features

*   **Automatic Organization:**
    *   By default, creates an album in Immich with the name of the Telegram group/chat.
    *   Supports manual album selection via message captions (see Usage).
*   **Secure Access:** The bot only processes files from chats where at least one of your specified Administrators is present.
*   **Feedback:** Provides status updates via message reactions:
    *   👌 — Successfully uploaded and added to the album.
    *   👀 — File already exists (duplicate ignored).
*   **Group Support:** Correctly handles Telegram media groups (albums), ensuring all files go to the designated target album.

## 🛠 Configuration

The bot is configured entirely via environment variables:

| Variable | Description |
| :--- | :--- |
| `TELEGRAM_TOKEN` | Your Telegram Bot Token (from @BotFather). |
| `IMMICH_URL` | The URL of your Immich server (e.g., `http://192.168.1.100:2283`). |
| `IMMICH_API_KEY` | Your Immich API Key (Settings -> API Keys). |
| `TELEGRAM_ADMINS` | Comma-separated list of User IDs allowed to authorize chats (e.g., `12345678,87654321`). |

> **How to get your User ID:** Message `@userinfobot` on Telegram.

## 🐳 Installation via Docker

You can easily run this bot using the pre-built Docker image.

### Option 1: Docker CLI

```bash
docker run -d \
  --name immichbot \
  --restart unless-stopped \
  -e TELEGRAM_TOKEN="your_telegram_token" \
  -e IMMICH_URL="http://your-immich-url:2283" \
  -e IMMICH_API_KEY="your_immich_api_key" \
  -e TELEGRAM_ADMINS="123456789" \
  ghcr.io/split174/immichman:v0.0.1
```

### Option 2: Docker Compose

Create a `docker-compose.yml` file:

```yaml
version: "3.8"
services:
  immichbot:
    image: ghcr.io/split174/immichman:v0.0.1
    container_name: immichbot
    restart: unless-stopped
    environment:
      - TELEGRAM_TOKEN=your_telegram_token
      - IMMICH_URL=http://192.168.1.100:2283
      - IMMICH_API_KEY=your_immich_api_key
      - TELEGRAM_ADMINS=123456789,987654321
```

Then run:
```bash
docker-compose up -d
```

## 🚀 Usage

1.  **Add the bot** to a group chat or send a message directly (you must be in the `TELEGRAM_ADMINS` list).
2.  **Send Media:**
    *   Send a photo or video.
    *   The bot will upload it to an Immich album named after the chat (e.g., *"Family Group"*).
3.  **Specify a Custom Album:**
    *   Add a caption to your media containing the trigger word `!папка` (Russian for "!folder") followed by the album name.
    *   Example caption: `Look at this view! !папка Trip 2024`
    *   The bot will upload the file(s) to the **"Trip 2024"** album in Immich (creating it if it doesn't exist).

## 🔒 Security & Privacy

*   **Authorization Cache:** To respect Telegram API limits, when an admin is detected in a chat, the bot caches "authorization" for that chat for 1 hour.
*   **Media Deduplication:** The bot generates a `deviceAssetId` based on the Telegram Message ID and Chat ID. If you try to upload the exact same message again, Immich will detect it as a duplicate, and the bot will react with 👀.