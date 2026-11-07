/**
 * The ledger page. The exit criterion is one sentence — *the audit list
 * answers who did what* — and the suite reads like that sentence: a name, a
 * verb, a target, and the server's answer, on every row. The refusals are
 * pinned as loudly as the successes, because they are the rows an operator
 * actually comes to read.
 */
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';

import client from '../api/client';
import type { AuditView } from '../api/audit';
import { AuthProvider } from '../auth/context';
import { axiosHarness, type Call, type Scripted } from '../test/axiosHarness';
import AuditPage from './AuditPage';

function row(over: Partial<AuditView> = {}): AuditView {
  return {
    id: 'a1',
    actor_user_id: 'u-alice',
    actor_username: 'alice',
    actor_roles: 'admin',
    actor_provider: 'local',
    action: 'camera.delete',
    target_type: 'camera',
    target_id: 'c1',
    outcome: 'ok',
    details: { name: 'front-door' },
    source_ip: '10.0.0.5',
    created_at: '2026-11-06T12:01:00Z',
    ...over,
  };
}

function attachHarness(script: (call: Call) => Scripted) {
  const { calls, adapter } = axiosHarness(script);
  const previous = client.defaults.adapter;
  client.defaults.adapter = adapter;
  return { calls, restore: () => (client.defaults.adapter = previous) };
}

function server(rows: AuditView[], over: Partial<Record<string, Scripted>> = {}) {
  return (call: Call): Scripted => {
    if (call.url.startsWith('/audit')) {
      if (over.audit) return over.audit;
      return {
        status: 200,
        data: { audits: rows, count: rows.length, limit: 50, offset: 0, truncated: false },
      };
    }
    return { status: 404, data: { error: `unexpected ${call.method} ${call.url}` } };
  };
}

const auditCalls = (calls: Call[]) => calls.filter((c) => c.url.startsWith('/audit'));

function renderAt(entry = '/audit', roles: string[] = ['admin']) {
  localStorage.setItem('nvr_roles', JSON.stringify(roles));
  localStorage.setItem('nvr_access_token', 'a-token');
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <AuthProvider>
        <Routes>
          <Route path="/audit" element={<AuditPage />} />
        </Routes>
      </AuthProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  localStorage.clear();
});

describe('AuditPage — who did what', () => {
  it('every row carries the name, the verb, the target, and the server’s answer', async () => {
    const h = attachHarness(
      server([
        row(),
        row({
          id: 'a2',
          actor_user_id: '',
          actor_username: 'bob',
          actor_roles: 'viewer',
          action: 'camera.delete',
          target_id: 'c2',
          outcome: 'refused',
          details: null,
          source_ip: '10.0.0.9',
          created_at: '2026-11-06T12:00:00Z',
        }),
      ]),
    );
    renderAt();

    expect(await screen.findByText('alice')).toBeTruthy();
    // Scoped to the rows: the filter select carries the same four words, and
    // an assertion that cannot tell them apart proves nothing.
    const list = screen.getByRole('list', { name: 'Ledger rows' });
    expect(within(list).getAllByText('camera.delete')).toHaveLength(2);
    expect(within(list).getByText('→ camera c1')).toBeTruthy();
    expect(within(list).getByText('ok')).toBeTruthy();
    expect(within(list).getByText('refused')).toBeTruthy();
    expect(within(list).getByText('bob')).toBeTruthy();
    // The details are the server's parsed map, rendered as written.
    expect(within(list).getByText('{"name":"front-door"}')).toBeTruthy();
    expect(within(list).getByText('from 10.0.0.5')).toBeTruthy();
    h.restore();
  });

  it('a row with no account still says who knocked', async () => {
    const h = attachHarness(
      server([row({ actor_username: '', actor_user_id: '', action: 'auth.login_failed' })]),
    );
    renderAt();
    expect(await screen.findByText('(no account)')).toBeTruthy();
    expect(screen.getByText('auth.login_failed')).toBeTruthy();
    h.restore();
  });

  it('an empty ledger says so, and does not dress it up', async () => {
    const h = attachHarness(server([]));
    renderAt();
    expect(await screen.findByText(/The ledger is empty/)).toBeTruthy();
    h.restore();
  });
});

describe('AuditPage — the filters are the address bar', () => {
  it('a filter travels to the request and stays in the open', async () => {
    const h = attachHarness(server([row()]));
    renderAt();
    await screen.findByText('alice');

    fireEvent.change(screen.getByLabelText('actor'), { target: { value: 'alice' } });
    fireEvent.change(screen.getByLabelText('outcome'), { target: { value: 'refused' } });
    fireEvent.click(screen.getByRole('button', { name: 'filter' }));

    await waitFor(() =>
      expect(
        auditCalls(h.calls).some((c) => c.url.includes('actor=alice') && c.url.includes('outcome=refused')),
      ).toBe(true),
    );
    expect(await screen.findByText('filtered by actor=alice · outcome=refused')).toBeTruthy();
    h.restore();
  });

  it('the outcome filter offers exactly the server’s four words', () => {
    const h = attachHarness(server([]));
    renderAt();

    const select = screen.getByLabelText('outcome') as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.value)).toEqual([
      '',
      'ok',
      'refused',
      'not_found',
      'error',
    ]);
    h.restore();
  });

  it('paging asks the server for the next page, in the URL', async () => {
    const h = attachHarness(
      server([row()], {
        audit: {
          status: 200,
          data: { audits: [row()], count: 50, limit: 50, offset: 0, truncated: true },
        },
      }),
    );
    renderAt();
    await screen.findByText('alice');

    fireEvent.click(screen.getByRole('button', { name: 'older' }));
    await waitFor(() =>
      expect(auditCalls(h.calls).some((c) => c.url.includes('offset=50'))).toBe(true),
    );
    h.restore();
  });
});

describe('AuditPage — the gate', () => {
  it('a viewer meets a refusal and fires no ledger request', async () => {
    const h = attachHarness(server([row()]));
    renderAt('/audit', ['viewer']);

    expect(await screen.findByText(/The ledger belongs to an admin/)).toBeTruthy();
    expect(auditCalls(h.calls)).toEqual([]);
    h.restore();
  });
});
