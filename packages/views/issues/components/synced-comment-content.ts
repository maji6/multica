// Display and copy use the body; editing, annotations, and API writes retain
// the original content, including the inert command and recovery receipt.
export function parseSyncedComment(content: string) {
  const match = content.match(/^\/note\n\n([\s\S]+)\n\n来源：(https:\/\/www\.tapd\.cn\/\d+\/bugtrace\/bugs\/view\/\d+)\n\[SaveReview:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:tapd:\d+\]\s*$/i);
  const body = match?.[1], source = match?.[2];
  if (!body || !source) return null;
  // Require the integration's author/time header too, rather than hiding an
  // arbitrary user's /note command or a receipt quoted inside ordinary prose.
  const header = body.match(/^([^\n]+) · (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\n/);
  if (!header) return null;
  return { author: header[1]!, createdAt: header[2]!, sourceUrl: source, body: body.slice(header[0].length).replace(/^\n/, "") };
}

export function syncedCommentContent(content: string): string {
  return parseSyncedComment(content)?.body ?? content;
}
