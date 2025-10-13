# 🌏 PHIVOLCS Earthquake Alert Tool for Matrix

A lightweight **Go** program that monitors the [PHIVOLCS Earthquake Information page](https://earthquake.phivolcs.dost.gov.ph/), detects **new** and **updated** earthquake reports (magnitude ≥ 4.5), and automatically sends alerts to a **Matrix** room.

Each alert includes:
- Date and time of the quake  
- Location and coordinates (with clickable **Google Maps link**)  
- Magnitude and depth  
- Update notice if PHIVOLCS revises magnitude

---

## ⚙️ Features

- 🔁 Detects both **new** and **updated** quake reports  
- 🌐 Posts formatted **HTML alerts** with emoji and bold text  
- 🗺️ Adds **Google Maps link** for each quake  
- 💾 Remembers previously processed events in a local cache file  
- ⏱️ Runs continuously every **150 seconds**

---

## 🧩 Requirements

- Go 1.20 or later  
- Internet access to PHIVOLCS and Matrix homeserver  
- A Matrix account and room (where the bot can post)

---

## 🔧 Environment Variables

Before running, set the following environment variables.  
These control how the notifier connects to Matrix and manages data.

| Variable | Required | Description | Example |
|-----------|-----------|-------------|----------|
| `MATRIX_BASE_URL` | ✅ | Matrix homeserver | `https://matrix.example.org` |
| `MATRIX_ACCESS_TOKEN` | ✅ | Matrix access token (Bearer token) | `syt_abcdefgh123456789` |
| `MATRIX_ROOM_ID` | ✅ | Matrix Room ID to which alerts are to be posted | `!roomid:example.org` |
| `PARSE_LIMIT` | ⛔ | Number of quake data to fetch (defaults to `100`) | `50` |

---

## 🪄 Installation

```bash
git clone https://github.com/vincejv/phivolcs-eq-to-matrix.git
cd phivolcs-eq-to-matrix
go build -o phivolcs-eq-to-matrix
