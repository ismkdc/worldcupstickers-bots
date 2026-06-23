# worldcup-bots

A small collection of Telegram stock-watcher bots that track **Panini FIFA World Cup 2026** sticker products (albums and sticker packets) across several Turkish retailers. Each bot polls a retailer's API/website on an interval, detects when an item transitions from *out of stock* to *in stock*, and pushes a notification to a Telegram chat, group, or forum topic.

All bots are written in **Go** ([`enetx/surf`](https://github.com/enetx/surf) for Chrome-impersonating HTTP) and ship as tiny [distroless](https://github.com/GoogleContainerTools/distroless) Docker images. State is persisted to a small JSON file so restarts don't replay old notifications.

## Bots

| Bot | Retailer | How it detects stock |
| --- | --- | --- |
| [`migros-watcher`](./migros-watcher) | [Migros](https://www.migros.com.tr) ("Tıkla Gel Al" / click & collect) | For each store: select the pick-point, read the product screen status, and if `IN_SALE`, add to cart to read the real stock amount. Scans every store across Turkey. |
| [`macrocenter-watcher`](./macrocenter-watcher) | [MacroCenter](https://www.macrocenter.com.tr) | Same flow as the Migros bot, against the MacroCenter storefront. |
| [`armagan-watcher`](./armagan-watcher) | [Armağan Oyuncak](https://www.armaganoyuncak.com.tr) | Parses the product page's schema.org JSON-LD `offers.availability` (`InStock` / `OutOfStock`). |
| [`toyzz-watcher`](./toyzz-watcher) | [Toyzz Shop](https://www.toyzzshop.com) | Reads each product's `virtual_stock` from the public product API. |

## How it works

Each bot follows the same loop:

1. On startup it sends a heartbeat message listing the products it is watching.
2. It scans the configured products/stores every `CHECK_INTERVAL_SECONDS`.
3. It keeps the last known stock per product (and per store, for the Migros/MacroCenter bots) in `data/state.json`.
4. When an item goes from `0` to `> 0`, it sends a Telegram alert with the product name, link, and (where available) quantity and store.

The Migros and MacroCenter bots additionally reset their baseline every day at 09:00 Turkey time, so the first scan after the daily reset reports the current snapshot instead of flooding alerts.

## Configuration

Configuration is entirely via environment variables (loaded from a `.env` file by Docker Compose). Copy the example and fill in your own Telegram credentials:

```bash
cd migros-watcher        # or macrocenter-watcher / armagan-watcher / toyzz-watcher
cp .env.example .env
# edit .env with your bot token and chat id
```

| Variable | Required | Description |
| --- | --- | --- |
| `TELEGRAM_BOT_TOKEN` | yes | Bot token from [@BotFather](https://t.me/BotFather). |
| `TELEGRAM_CHAT_ID` | yes | Target chat/group/channel id (use the `-100…` form for supergroups & channels). |
| `TELEGRAM_THREAD_ID` | no | Forum/group topic id; if set, messages are posted into that topic. |
| `CHECK_INTERVAL_SECONDS` | no | Seconds between scans (defaults vary per bot). |
| `WORKERS` | no | Concurrent workers (Migros/MacroCenter only). |

> The `data/` directory and your `.env` file are git-ignored — secrets and runtime state never get committed.

## Running

Each bot is self-contained. With Docker Compose:

```bash
cd migros-watcher
cp .env.example .env     # then edit it
docker compose up -d --build
docker compose logs -f
```

The build pulls the latest Go toolchain and the latest `enetx/surf` + `enetx/g`, compiles a static binary, and copies it into a distroless runtime image. Runtime state lives in `./data` on the host.

## Disclaimer

These bots are for personal use and educational purposes. They only read publicly available stock information and send notifications — they do not place orders. Be considerate with the request interval and respect each retailer's terms of service.
