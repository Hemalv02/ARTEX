"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * "New version" indicator in the top bar: checked once on page load. If an update
 * is available, a badge lights up next to the version number; clicking it jumps
 * straight to the "Version & Update" card on the settings page.
 *
 * The backend caches GitHub query results for 30 minutes, so it is safe to check
 * on every mount. The unauthenticated GitHub API only allows 60 requests/hour/IP,
 * so without that cache, opening a few extra tabs would burn the quota and the
 * next genuine update check would fail.
 *
 * Failures are silenced: the top bar is not the place to surface errors. The
 * user will see the reason in Settings under "Check for updates".
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already implies that the version numbers are comparable; dev builds will never light this badge.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Silenced: no network or GitHub rate-limiting should not surface as an error in the top bar.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`New version ${latest} available — click to update`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* Pulsing dot: the top bar has many elements, so plain text is easy to miss — the animation makes it visible at a glance. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">Update {latest}</span>
      <span className="sm:hidden">Update</span>
    </Link>
  );
}
