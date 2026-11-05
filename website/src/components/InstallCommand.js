import React, {useEffect, useRef, useState} from 'react';
import clsx from 'clsx';
import {Check, Copy} from 'lucide-react';

const COMMANDS = [
  'helm repo add orka https://orka-agents.github.io/orka/charts',
  'helm repo update orka',
  'helm install orka orka/orka -n orka-system --create-namespace --wait --timeout 10m',
];

export default function InstallCommand({className, style}) {
  const [copied, setCopied] = useState(false);
  const timer = useRef(null);

  useEffect(() => () => clearTimeout(timer.current), []);

  const copy = () => {
    const text = COMMANDS.join('\n');
    const done = () => {
      setCopied(true);
      clearTimeout(timer.current);
      timer.current = setTimeout(() => setCopied(false), 1800);
    };
    if (navigator.clipboard?.writeText) {
      navigator.clipboard.writeText(text).then(done, () => {});
    }
  };

  return (
    <div className={clsx('orka-install', className)} style={style}>
      <div className="orka-install__bar">
        <span>Install</span>
        <button type="button" className="orka-install__copy" onClick={copy}>
          {copied ? <Check size={14} aria-hidden="true" /> : <Copy size={14} aria-hidden="true" />}
          <span>{copied ? 'Copied' : 'Copy'}</span>
        </button>
      </div>
      <pre className="orka-install__code">
        {COMMANDS.map((command) => (
          <span key={command}>
            <span className="orka-install__prompt" aria-hidden="true">
              ${' '}
            </span>
            {command}
            {'\n'}
          </span>
        ))}
      </pre>
    </div>
  );
}
