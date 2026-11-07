import { API_URL, ADMIN_PASSWORD, CAMERA_ID } from './playwright.config';

export default async function guardStack() {
  // Fail here, in one line, rather than in five specs each waiting on a
  // connection that will never open.
  let res: Response;
  try {
    res = await fetch(`${API_URL}/health`);
  } catch {
    throw new Error(`e2e: the stack is not answering at ${API_URL} — run scripts/e2e-stack.sh restart first`);
  }
  if (!res.ok) throw new Error(`e2e: ${API_URL}/health answered ${res.status}`);
  if (!ADMIN_PASSWORD) throw new Error('e2e: BOOTSTRAP_ADMIN_PASSWORD is empty — run the specs through scripts/e2e-stack.sh test');
  if (!CAMERA_ID) throw new Error('e2e: E2E_CAMERA_ID is empty — the seeded camera is what live/events/cameras specs run against');
}
