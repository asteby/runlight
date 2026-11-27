#!/bin/sh
# Copy the canonical SVGs from the app tree into brand/, so the files at hand
# never drift from the ones the dashboard serves. PNGs are separate: see README.
set -e
cd "$(dirname "$0")"
SRC=../internal/dashboard/web
for f in logo-light.svg logo-dark.svg logo-lockup.svg logo-lockup-dark.svg; do
  cp "$SRC/$f" "$f"
  echo "synced $f"
done
