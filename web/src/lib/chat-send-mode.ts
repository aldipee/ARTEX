"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// Send / newline keybindings for the session input box. Pure frontend preference:
// only stored in localStorage, not in the DB and not synced across accounts, so it
// must be reset when switching browsers. See issue #39 — 0.3.2 changed Ctrl+Enter
// send to Enter send; here we restore the old keybind as an option.
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter to send, Shift+Enter for newline" },
  { value: "ctrl-enter", label: "Ctrl+Enter to send, Enter for newline" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// Subscribers within the same tab. localStorage's `storage` event only fires in
// *other* tabs, so after changing the setting here we must emit to notify the input
// box in this tab, otherwise it would require a refresh to take effect.
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// Returns a string literal, compared by value with Object.is, so it won't trap useSyncExternalStore in a loop.
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// No localStorage on the server; render the default first, then getSnapshot corrects it after hydration.
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey decides whether a keypress should send.
// isComposing / keyCode 229 means an IME (e.g. Chinese) is still composing, which
// must be allowed through, otherwise confirming a candidate via Enter would mis-send.
// enter mode only excludes Shift, matching 0.3.2 behavior exactly — users who didn't
// change the setting keep the same feel. ctrl-enter mode accepts both Ctrl and Cmd (macOS).
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
