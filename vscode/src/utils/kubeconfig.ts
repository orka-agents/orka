import * as fs from 'fs';
import * as path from 'path';
import * as os from 'os';

interface KubeConfig {
  clusters?: Array<{
    name: string;
    cluster: {
      server: string;
      'certificate-authority-data'?: string;
    };
  }>;
  contexts?: Array<{
    name: string;
    context: {
      cluster: string;
      namespace?: string;
      user?: string;
    };
  }>;
  'current-context'?: string;
  users?: Array<{
    name: string;
    user: {
      token?: string;
      'client-certificate-data'?: string;
      'client-key-data'?: string;
    };
  }>;
}

/**
 * Try to detect Orka connection info from kubeconfig
 */
export async function detectFromKubeconfig(): Promise<{
  apiEndpoint?: string;
  authToken?: string;
  namespace?: string;
} | null> {
  try {
    const kubeconfig = loadKubeconfig();
    if (!kubeconfig) {
      return null;
    }

    const currentContext = kubeconfig['current-context'];
    if (!currentContext) {
      return null;
    }

    const context = kubeconfig.contexts?.find(c => c.name === currentContext);
    if (!context) {
      return null;
    }

    const cluster = kubeconfig.clusters?.find(c => c.name === context.context.cluster);
    const user = kubeconfig.users?.find(u => u.name === context.context.user);

    return {
      apiEndpoint: cluster?.cluster?.server,
      authToken: user?.user?.token,
      namespace: context.context.namespace || 'default',
    };
  } catch (err) {
    console.error('Failed to parse kubeconfig:', err);
    return null;
  }
}

/**
 * Load and parse kubeconfig file
 */
function loadKubeconfig(): KubeConfig | null {
  const kubeconfigPath = process.env.KUBECONFIG || path.join(os.homedir(), '.kube', 'config');

  if (!fs.existsSync(kubeconfigPath)) {
    return null;
  }

  const content = fs.readFileSync(kubeconfigPath, 'utf-8');

  // Simple YAML parser for kubeconfig (avoid dependency on js-yaml)
  // kubeconfig is structured enough that we can parse the key fields
  return parseSimpleYaml(content);
}

/**
 * Minimal YAML parser sufficient for kubeconfig
 * For production use, consider adding js-yaml as a dependency
 */
function parseSimpleYaml(content: string): KubeConfig | null {
  try {
    // Use JSON if the file is JSON format
    if (content.trim().startsWith('{')) {
      return JSON.parse(content);
    }

    // For YAML kubeconfig, we'll parse the essential fields
    // This is a simplified parser — covers the common kubeconfig format
    const result: KubeConfig = {};
    const lines = content.split('\n');
    let currentSection = '';
    let currentItem: any = null;
    let currentSubSection = '';
    const clusters: any[] = [];
    const contexts: any[] = [];
    const users: any[] = [];

    for (const line of lines) {
      const trimmed = line.trimEnd();
      if (!trimmed || trimmed.startsWith('#')) { continue; }

      const indent = line.length - line.trimStart().length;

      if (indent === 0 && trimmed.endsWith(':')) {
        currentSection = trimmed.slice(0, -1);
        currentItem = null;
        continue;
      }

      if (indent === 0 && trimmed.includes(': ')) {
        const [key, ...valueParts] = trimmed.split(': ');
        const value = valueParts.join(': ').trim();
        if (key === 'current-context') {
          result['current-context'] = value;
        }
        continue;
      }

      if (trimmed.startsWith('- name: ')) {
        currentItem = { name: trimmed.slice(8).trim() };
        if (currentSection === 'clusters') { clusters.push(currentItem); }
        else if (currentSection === 'contexts') { contexts.push(currentItem); }
        else if (currentSection === 'users') { users.push(currentItem); }
        currentSubSection = '';
        continue;
      }

      if (currentItem && indent > 0) {
        const kv = trimmed.trim();
        if (kv.endsWith(':') && !kv.includes(': ')) {
          currentSubSection = kv.slice(0, -1);
          if (!currentItem[currentSubSection]) {
            currentItem[currentSubSection] = {};
          }
          continue;
        }
        if (kv.includes(': ')) {
          const [key, ...valueParts] = kv.split(': ');
          const value = valueParts.join(': ').trim().replace(/^["']|["']$/g, '');
          if (currentSubSection && currentItem[currentSubSection]) {
            currentItem[currentSubSection][key.trim()] = value;
          }
        }
      }
    }

    if (clusters.length) {
      result.clusters = clusters.map(c => ({ name: c.name, cluster: c.cluster || {} }));
    }
    if (contexts.length) {
      result.contexts = contexts.map(c => ({ name: c.name, context: c.context || {} }));
    }
    if (users.length) {
      result.users = users.map(u => ({ name: u.name, user: u.user || {} }));
    }

    return result;
  } catch {
    return null;
  }
}
