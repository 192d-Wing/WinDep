export function humanSize(n: number): string {
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${u[i]}`;
}

// join a category + prefix + optional leaf into an /api path with each segment encoded.
export function apiPath(base: string, category: string, prefix: string, leaf?: string): string {
  const segs = [category, ...prefix.split("/").filter(Boolean)];
  if (leaf) segs.push(leaf);
  return `/api/${base}/${segs.map(encodeURIComponent).join("/")}`;
}
