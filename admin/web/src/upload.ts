// Resumable upload client. It mirrors the admin backend's offset protocol: HEAD the
// target to learn how many bytes are already durable, then PATCH the remaining slice.
// If the connection drops, it re-HEADs and resumes from the server's confirmed offset
// instead of restarting — so a dropped multi-GB WIM upload picks up where it left off.

const OFFSET = "Upload-Offset";
const LENGTH = "Upload-Length";
const COMPLETE = "Upload-Complete";
const MAX_ATTEMPTS = 6;

interface State {
  offset: number;
  complete: boolean;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function num(v: string | null, fallback: number): number {
  const n = Number(v);
  return Number.isFinite(n) && n >= 0 ? n : fallback;
}

// head reports how many bytes the server already holds for this target.
async function head(url: string): Promise<State> {
  const r = await fetch(url, { method: "HEAD" });
  return { offset: num(r.headers.get(OFFSET), 0), complete: r.headers.get(COMPLETE) === "1" };
}

// patch streams file.slice(offset)..end and resolves with the server's post-write state.
function patch(
  url: string,
  file: File,
  offset: number,
  onBytes: (uploaded: number) => void,
): Promise<State & { status: number }> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PATCH", url);
    xhr.setRequestHeader(OFFSET, String(offset));
    xhr.setRequestHeader(LENGTH, String(file.size));
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onBytes(offset + e.loaded);
    };
    xhr.onload = () =>
      resolve({
        status: xhr.status,
        offset: num(xhr.getResponseHeader(OFFSET), offset),
        complete: xhr.getResponseHeader(COMPLETE) === "1",
      });
    xhr.onerror = () => reject(new Error("network error"));
    xhr.onabort = () => reject(new Error("aborted"));
    xhr.send(file.slice(offset));
  });
}

// resumableUpload uploads file to url, invoking onBytes with the running total. It
// tolerates transient drops with bounded, backing-off retries that resync the offset.
export async function resumableUpload(
  url: string,
  file: File,
  onBytes: (uploaded: number) => void,
): Promise<void> {
  let state = await head(url);
  if (state.complete) {
    onBytes(file.size);
    return;
  }

  for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt++) {
    try {
      const res = await patch(url, file, state.offset, onBytes);
      if (res.status >= 200 && res.status < 300) {
        if (res.complete || res.offset >= file.size) {
          onBytes(file.size);
          return;
        }
        state.offset = res.offset; // partial accepted — loop to send the remainder
        continue;
      }
      if (res.status !== 409) throw new Error(`${file.name}: HTTP ${res.status}`);
      // 409 offset mismatch: fall through to resync (no backoff — it's not an error).
    } catch (e) {
      if (attempt === MAX_ATTEMPTS - 1) throw e instanceof Error ? e : new Error(String(e));
      await sleep(Math.min(1000 * 2 ** attempt, 8000));
    }
    state = await head(url); // resync the offset before the next attempt
    if (state.complete) {
      onBytes(file.size);
      return;
    }
  }
  throw new Error(`${file.name}: upload did not complete`);
}
