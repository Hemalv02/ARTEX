"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * The top bar's "new version available" hint: queried once on full page load, and if there is an update it lights up next to the version number,
 * clicking which goes straight to the "Version & Updates" card on the System Config page.
 *
 * The backend caches GitHub query results for 30 minutes, so querying once on every mount here is safe
 * — the unauthenticated GitHub API is only 60/hour/IP, and without that cache a few open tabs
 * would exhaust the quota, leaving you unable to query when you actually want to update.
 *
 * Query failures are always silent: the top bar is not the place for errors; the user sees the reason by clicking "Check for updates" on the settings page.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already includes the "versions are comparable" check, so a dev build never lights this hint.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Silent: neither no network nor GitHub rate limiting should pop an error in the top bar.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`New version ${latest} available — click to go update`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* Breathing dot: the top bar has many elements, and plain text is easy to miss, so the animation makes it noticeable at a glance. */}
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
