// Presentation only. Editing, copying, annotations, and API writes continue to
// use the original content, including the inert command and recovery receipt.
export function syncedCommentContent(content: string): string {
  const match = content.match(/^\/note\n\n([\s\S]+)\n\n来源：(https:\/\/www\.tapd\.cn\/\d+\/bugtrace\/bugs\/view\/\d+)\n\[SaveReview:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:tapd:\d+\]\s*$/i);
  const body = match?.[1], source = match?.[2];
  if (!body || !source) return content;
  // Require the integration's author/time header too, rather than hiding an
  // arbitrary user's /note command or a receipt quoted inside ordinary prose.
  if (!/^[^\n]+ · \d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\n/.test(body)) return content;
  return `${body}\n\n[TAPD](${source})`;
}
