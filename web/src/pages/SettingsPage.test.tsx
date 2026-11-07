/**
 * The settings page. What is pinned here is the card's four exit criteria, and
 * the one rule underneath all of them: **the page shows the server's state, not
 * a spinner, and never a secret.**
 *
 * The connection test is the loudest one — the four named states must reach the
 * operator as those exact words, because `discovery_unreachable` and `bucket
 * missing` are different fixes and a page that flattened them into "failed"
 * sends the operator hunting in the wrong place. The secret rule is the
 * quietest and the most load-bearing: the form renders *set / unset*, an
 * untouched write-only input is **omitted** from the save body (omitted keeps,
 * empty clears), and a suite that let a spread through would wipe a password
 * the operator never touched.
 */
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';

import client from '../api/client';
import type {
  OIDCProviderView,
  StorageBackendView,
  WebhookDeliveryView,
  WebhookView,
} from '../api/settings';
import { AuthProvider } from '../auth/context';
import { useRealtime } from '../stores/realtime';
import { axiosHarness, type Call, type Scripted } from '../test/axiosHarness';
import SettingsPage from './SettingsPage';

function backend(over: Partial<StorageBackendView> = {}): StorageBackendView {
  return {
    id: 'b1',
    name: 'tier-one',
    kind: 's3',
    endpoint: 'https://minio.lan:9000',
    region: 'us-east-1',
    bucket: 'nvr',
    prefix: 'footage',
    username: '',
    insecure_tls: false,
    root_path: '',
    enabled: true,
    is_default: true,
    secret_set: false,
    password_set: false,
    access_key_set: true,
    secret_key_set: true,
    private_key_path_set: false,
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-06T00:00:00Z',
    ...over,
  };
}

function provider(over: Partial<OIDCProviderView> = {}): OIDCProviderView {
  return {
    id: 'p1',
    name: 'corp',
    type: 'oidc',
    issuer: 'https://id.example.test/realms/nvr',
    discovery_url: '',
    client_id: 'nvr-console',
    secret_set: true,
    scope: 'openid profile email',
    enabled: true,
    redirect_uri: 'http://localhost:3000/api/auth/sso/corp/callback',
    auth_url: 'https://id.example.test/authorize',
    token_url: 'https://id.example.test/token',
    user_info_url: '',
    jwks_uri: 'https://id.example.test/jwks',
    discovery_state: 'ok',
    pkce_supported: true,
    discovered_at: '2026-11-06T00:00:00Z',
    link_policy: 'pending',
    allowed_domains: '',
    claims_email: 'email',
    claims_username: 'preferred_username',
    claims_name: 'name',
    claims_groups: 'groups',
    require_verified_email: true,
    sort_order: 0,
    ...over,
  };
}

function hook(over: Partial<WebhookView> = {}): WebhookView {
  return {
    id: 'w1',
    name: 'notify-home',
    url: 'https://hooks.example.test/nvr',
    camera_id: '',
    events: ['event.close'],
    enabled: true,
    secret_set: true,
    last_error: 'dial tcp: connection refused',
    last_delivered_at: null,
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-06T00:00:00Z',
    ...over,
  };
}

const QUEUE = { pending: 3, uploading: 1, failed: 2, uploaded: 40, last_error: '' };

const RETENTION = {
  local_days: 14,
  remote_days: 0,
  remote_forever: true,
  event_days: 90,
  unuploaded_protection_days: 42,
  webhook_delivery_days: 30,
  audit_days: 365,
  job_days: 30,
  author: 'environment',
};

function attachHarness(script: (call: Call) => Scripted) {
  const { calls, adapter } = axiosHarness(script);
  const previous = client.defaults.adapter;
  client.defaults.adapter = adapter;
  return { calls, restore: () => (client.defaults.adapter = previous) };
}

function server(over: Partial<Record<string, Scripted>> = {}) {
  return (call: Call): Scripted => {
    const key = `${call.method} ${call.url}`;
    if (over[key]) return over[key] as Scripted;
    switch (key) {
      case 'get /storage/backends':
        return { status: 200, data: { backends: [backend()], count: 1 } };
      case 'get /storage/queue':
        return {
          status: 200,
          data: { jobs: [], count: 0, state: '', limit: 50, offset: 0, truncated: false, counts: QUEUE },
        };
      case 'post /storage/backends/b1/test':
        return { status: 200, data: { ok: true, kind: 's3', name: 'tier-one' } };
      case 'put /storage/backends/b1':
        return { status: 200, data: backend() };
      case 'put /storage/backends/b1/enabled':
        return { status: 200, data: { name: 'tier-one', enabled: false, is_default: true } };
      case 'delete /storage/backends/b1':
        return { status: 200, data: { deleted: 'tier-one', orphaned_jobs: 7 } };
      case 'get /settings/oidc':
        return { status: 200, data: { providers: [provider()] } };
      case 'get /settings/webhooks':
        return { status: 200, data: { webhooks: [hook()], count: 1 } };
      case 'get /settings/webhooks/w1/deliveries':
        return { status: 200, data: { deliveries: deliveries(), count: 2 } };
      case 'get /settings/retention':
        return { status: 200, data: RETENTION };
      case 'get /v1/settings/media':
        // P-11: the roll and the segment length, read through the same door
        // the drawer's strip pads by. Non-defaults so a restated default in
        // the panel would show as a mismatch.
        return { status: 200, data: { pre_roll_seconds: 8, post_roll_seconds: 15, segment_seconds: 5 } };
      default:
        return { status: 404, data: { error: `unexpected ${call.method} ${call.url}` } };
    }
  };
}

function deliveries(): WebhookDeliveryView[] {
  return [
    {
      id: 'd2',
      event: 'event.close',
      status: 'delivered',
      status_code: 200,
      attempt: 2,
      error: '',
      delivered_at: '2026-11-06T10:00:01Z',
      created_at: '2026-11-06T10:00:01Z',
      payload: {},
    },
    {
      id: 'd1',
      event: 'event.close',
      status: 'failed',
      status_code: 500,
      attempt: 1,
      error: 'the receiver said 500',
      delivered_at: null,
      created_at: '2026-11-06T10:00:00Z',
      payload: {},
    },
  ];
}

function renderAt(entry = '/settings', roles: string[] = ['admin']) {
  localStorage.setItem('nvr_roles', JSON.stringify(roles));
  localStorage.setItem('nvr_access_token', 'a-token');
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <AuthProvider>
        <Routes>
          <Route path="/settings" element={<SettingsPage />} />
        </Routes>
      </AuthProvider>
    </MemoryRouter>,
  );
}

async function openTab(name: 'Access' | 'Webhooks' | 'Retention') {
  fireEvent.click(await screen.findByRole('button', { name }));
}

beforeEach(() => {
  act(() => useRealtime.setState({ storageQueue: null }));
});

describe('SettingsPage — the storage tab', () => {
  it('renders the queue counts from the read', async () => {
    const h = attachHarness(server());
    renderAt();

    const strip = await screen.findByRole('region', { name: 'Upload queue' });
    expect(within(strip).getByText('pending 3')).toBeTruthy();
    expect(within(strip).getByText('uploading 1')).toBeTruthy();
    expect(within(strip).getByText('failed 2')).toBeTruthy();
    expect(within(strip).getByText('uploaded 40')).toBeTruthy();
    h.restore();
  });

  it('follows the storage.queue feed when the sweep reports', async () => {
    const h = attachHarness(server());
    renderAt();
    const strip = await screen.findByRole('region', { name: 'Upload queue' });
    expect(within(strip).getByText('pending 3')).toBeTruthy();

    act(() =>
      useRealtime.setState({
        storageQueue: {
          force_deleted: 1,
          queue: { pending: 9, uploading: 0, failed: 0, uploaded: 41, last_error: '' },
        },
      }),
    );
    expect(await within(strip).findByText('pending 9')).toBeTruthy();
    // The number that matters: un-uploaded footage destroyed past the window.
    expect(within(strip).getByText(/force deleted: 1/)).toBeTruthy();
    h.restore();
  });

  it('renders each named connection state verbatim, because each is a different fix', async () => {
    const states = [
      'discovery_unreachable',
      'bad credentials',
      'sftp host key changed',
      'bucket missing',
    ];
    for (const state of states) {
      const h = attachHarness(
        server({
          'post /storage/backends/b1/test': {
            status: 200,
            data: { ok: false, error: state, reason: 'the driver sentence' },
          },
        }),
      );
      const view = renderAt();
      fireEvent.click(await screen.findByRole('button', { name: 'test connection' }));
      expect(await screen.findByText(`test: ${state}`)).toBeTruthy();
      // Unmount between states: the next iteration must find exactly one
      // button, and a page left mounted keeps polling the harness.
      view.unmount();
      h.restore();
    }
  });

  it('renders ok when the tier answers ok', async () => {
    const h = attachHarness(server());
    renderAt();
    fireEvent.click(await screen.findByRole('button', { name: 'test connection' }));
    expect(await screen.findByText('test: ok')).toBeTruthy();
    h.restore();
  });

  it('shows secrets as set or unset, never as values', async () => {
    const h = attachHarness(server());
    renderAt();
    fireEvent.click(await screen.findByRole('button', { name: 'edit' }));

    expect(await screen.findByText('access_key — set')).toBeTruthy();
    expect(await screen.findByText('secret_key — set')).toBeTruthy();
    // The write-only inputs start empty: the page has no value to show, and
    // the server never sent one.
    for (const input of screen.getAllByPlaceholderText('already set — type to replace')) {
      expect((input as HTMLInputElement).value).toBe('');
    }
    h.restore();
  });

  it('saves only what changed, and never an untouched secret', async () => {
    const h = attachHarness(server());
    renderAt();
    fireEvent.click(await screen.findByRole('button', { name: 'edit' }));

    const name = await screen.findByLabelText('name');
    fireEvent.change(name, { target: { value: 'tier-two' } });
    fireEvent.click(screen.getByRole('button', { name: 'save backend' }));

    await waitFor(() => {
      const put = h.calls.find((c) => c.method === 'put' && c.url === '/storage/backends/b1');
      expect(put).toBeTruthy();
    });
    const put = h.calls.find((c) => c.method === 'put' && c.url === '/storage/backends/b1');
    const body = put?.body as Record<string, unknown>;
    expect(body.name).toBe('tier-two');
    // The omitted-vs-empty rule: an untouched secret is absent, because on
    // this API absent keeps and empty clears.
    expect('access_key' in body).toBe(false);
    expect('secret_key' in body).toBe(false);
    expect('password' in body).toBe(false);
    expect('private_key_path' in body).toBe(false);
    // And the flags that have their own routes stay out of the row write.
    expect('kind' in body).toBe(false);
    expect('enabled' in body).toBe(false);
    expect('is_default' in body).toBe(false);
    h.restore();
  });

  it('delete reports the queue rows that pointed at the destination', async () => {
    const h = attachHarness(server());
    renderAt();
    fireEvent.click(await screen.findByRole('button', { name: 'delete' }));
    expect(await screen.findByText(/7 queue row\(s\) pointed at it and stay/)).toBeTruthy();
    h.restore();
  });

  it('the add form offers the four kinds, and fs is addressed by a path alone', async () => {
    const h = attachHarness(server());
    renderAt();
    fireEvent.click(await screen.findByRole('button', { name: 'add a backend' }));

    const kind = await screen.findByLabelText('kind');
    expect(within(kind as HTMLSelectElement).getAllByRole('option').map((o) => o.textContent)).toEqual(
      ['s3', 'webdav', 'sftp', 'fs'],
    );
    fireEvent.change(kind, { target: { value: 'fs' } });
    expect(await screen.findByLabelText('root_path')).toBeTruthy();
    expect(screen.queryByLabelText('endpoint')).toBeNull();
    h.restore();
  });
});

describe('SettingsPage — the access tab', () => {
  it('shows the secret flag, the discovery state, and the redirect uri to whitelist', async () => {
    const h = attachHarness(server());
    renderAt();
    await openTab('Access');

    expect(await screen.findByText('client secret: set')).toBeTruthy();
    expect(screen.getByText('discovery: ok')).toBeTruthy();
    expect(screen.getByText(/redirect uri .*http:\/\/localhost:3000\/api\/auth\/sso\/corp\/callback/)).toBeTruthy();
    h.restore();
  });

  it('a discovery refusal renders the named state, not a generic failure', async () => {
    const h = attachHarness(
      server({
        'post /settings/oidc': {
          status: 422,
          data: {
            error: 'provider refused by discovery',
            discovery: 'issuer_mismatch',
            discovery_reason: 'the document names https://elsewhere.example.test',
          },
        },
      }),
    );
    renderAt();
    await openTab('Access');

    fireEvent.click(await screen.findByRole('button', { name: 'add a provider' }));
    fireEvent.change(await screen.findByLabelText('name'), { target: { value: 'second' } });
    fireEvent.change(screen.getByLabelText('issuer'), {
      target: { value: 'https://id.example.test/realms/nvr' },
    });
    fireEvent.change(screen.getByLabelText('client id'), { target: { value: 'nvr-console' } });
    fireEvent.change(screen.getByPlaceholderText('not set'), { target: { value: 'a-secret' } });
    fireEvent.click(screen.getByRole('button', { name: 'add provider' }));

    expect(
      await screen.findByText(/discovery: issuer_mismatch — the document names/),
    ).toBeTruthy();
    h.restore();
  });
});

describe('SettingsPage — the webhooks tab', () => {
  it('shows the secret flag and the last outcome in one line', async () => {
    const h = attachHarness(server());
    renderAt();
    await openTab('Webhooks');

    expect(await screen.findByText('secret: set')).toBeTruthy();
    expect(screen.getByText('every camera')).toBeTruthy();
    expect(screen.getByText(/last error: dial tcp: connection refused/)).toBeTruthy();
    h.restore();
  });

  it('the delivery history renders the attempt ladder', async () => {
    const h = attachHarness(server());
    renderAt();
    await openTab('Webhooks');

    fireEvent.click(await screen.findByRole('button', { name: 'delivery history' }));
    expect(await screen.findByText('attempt 1: failed (500)')).toBeTruthy();
    expect(screen.getByText('attempt 2: delivered (200)')).toBeTruthy();
    expect(screen.getByText('the receiver said 500')).toBeTruthy();
    h.restore();
  });
});

describe('SettingsPage — the retention tab', () => {
  it('renders the windows, and forever means forever', async () => {
    const h = attachHarness(server());
    renderAt();
    await openTab('Retention');

    expect(await screen.findByText('keep forever')).toBeTruthy();
    expect(screen.getByText('42 days')).toBeTruthy();
    expect(screen.getByText('90 days')).toBeTruthy();
    // Read-only by contract: the environment is the policy's only author.
    expect(screen.queryByRole('button', { name: /save/i })).toBeNull();
    expect(screen.getByText(/authored by the environment/)).toBeTruthy();
    h.restore();
  });

  it('shows the roll and the segment length read from the media route (P-11)', async () => {
    // The panel does not restate these defaults: the numbers on screen are
    // the ones the clip builder and the recorder run on, from the same
    // answer the drawer's strip pads by.
    const h = attachHarness(server());
    renderAt();
    await openTab('Retention');
    await screen.findByText('keep forever');

    expect(screen.getByText('clip pre-roll')).toBeTruthy();
    expect(screen.getByText('8s')).toBeTruthy();
    expect(screen.getByText('15s')).toBeTruthy();
    expect(screen.getByText('clip post-roll')).toBeTruthy();
    expect(screen.getByText('segment length')).toBeTruthy();
    expect(screen.getByText(/PRE_ROLL_SECONDS/)).toBeTruthy();
    h.restore();
  });
});

describe('SettingsPage — the gate', () => {
  it('a viewer sees no tabs and asks the server for nothing', async () => {
    const h = attachHarness(server());
    renderAt('/settings', ['viewer']);

    expect(await screen.findByText(/Settings belong to an admin/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Storage' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Access' })).toBeNull();
    // The refusal is drawn before any request: a viewer never fires the four
    // reads that would each answer 403.
    expect(h.calls.filter((c) => c.url.startsWith('/storage') || c.url.startsWith('/settings'))).toEqual([]);
    h.restore();
  });
});
