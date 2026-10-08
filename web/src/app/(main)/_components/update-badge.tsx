"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * "New version" badge in the top bar: checked once on full page load; when an update
 * is available it lights up next to the version number, clicking through to the
 * "Version & update" card on the system settings page.
 *
 * The backend caches its GitHub query result for 30 minutes, so checking once per
 * mount here is safe — the unauthenticated GitHub API is limited to 60 req/hour/IP,
 * and without that cache a few open tabs would burn through the quota, after which a
 * real update couldn't even be queried.
 *
 * Any query failure is silent: the top bar is not the place to surface errors; the
 * user will see the reason after clicking "Check for updates" on the settings page.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already incorporates the "version numbers are comparable" check; dev builds won't show this badge.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Silent: no network / GitHub rate limit shouldn't pop an error in the top bar.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`New version ${latest} available, click to update`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* Breathing dot: the top bar has many elements and plain text is easy to miss, the animation makes it visible at a glance. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">New version {latest}</span>
      <span className="sm:hidden">New version</span>
    </Link>
  );
}
