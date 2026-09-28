import { useCallback, useEffect, useMemo, useState } from "react";

// useCatalogChoices holds a list of things the user can turn on and off, such as
// their own skills or MCP servers. The list is the same for every agent, and
// choices are remembered once per browser, then resent on every run and resume.
//
// Items not `optional` (the developer's global servers, say) are always on, so a
// stale choice for one is never sent. Nothing is loaded while `enabled` is false.
export function useCatalogChoices<T extends { name: string }>({
  enabled,
  storageKey,
  load,
  optional,
}: {
  enabled: boolean;
  storageKey: string;
  // Must be stable, such as a module-level function.
  load: () => Promise<T[]>;
  // Must be stable, such as a module-level function. Defaults to every item.
  optional?: (item: T) => boolean;
}) {
  const [items, setItems] = useState<T[]>([]);
  const [error, setError] = useState("");
  const [choices, setChoices] = useState<Record<string, boolean>>({});
  const [revision, setRevision] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setError("");
    let saved: Record<string, boolean> = {};
    try { saved = JSON.parse(localStorage.getItem(storageKey) || "{}"); } catch { /* storage unavailable */ }
    setChoices(saved);
    if (!enabled) {
      setItems([]);
      return;
    }
    load().then(loaded => {
      if (!cancelled) setItems(loaded);
    }).catch(err => { if (!cancelled) setError(String(err)); });
    return () => { cancelled = true; };
  }, [enabled, storageKey, load, revision]);

  const toggle = (name: string, checked: boolean) => {
    const next = { ...choices, [name]: checked };
    setChoices(next);
    try { localStorage.setItem(storageKey, JSON.stringify(next)); } catch { /* storage unavailable */ }
  };

  // What to send: the items turned off, among those that can be.
  const disabled = useMemo(() => {
    const switchable = new Set(items.filter(item => optional?.(item) ?? true).map(item => item.name));
    return Object.entries(choices).filter(([name, on]) => !on && switchable.has(name)).map(([name]) => name);
  }, [items, choices, optional]);

  // Reloads the list, after the user adds or removes an item.
  const reload = useCallback(() => setRevision(value => value + 1), []);

  return { items, choices, error, toggle, disabled, reload };
}
