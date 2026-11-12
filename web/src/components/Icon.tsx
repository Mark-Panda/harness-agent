import type { ReactNode } from "react";

const paths = {
  logo: <><path d="M5 4v16M19 4v16M5 9h8l6 6M5 15h6l4-4" /><path d="M3 4h4M17 4h4M3 20h4M17 20h4" /></>,
  dashboard: <><rect x="3" y="3" width="7" height="7" rx="1" /><rect x="14" y="3" width="7" height="7" rx="1" /><rect x="3" y="14" width="7" height="7" rx="1" /><rect x="14" y="14" width="7" height="7" rx="1" /></>,
  chat: <path d="M21 11.5a8.5 8.5 0 0 1-8.5 8.5H4l1.7-4.1a8.5 8.5 0 1 1 15.3-4.4Z" />,
  agent: <><rect x="4" y="7" width="16" height="13" rx="3" /><path d="M12 7V3m-2 0h4M2 12v3m20-3v3M8 12h.01m8 0h.01M9 16h6" /></>,
  memory: <><circle cx="12" cy="5" r="2" /><circle cx="5" cy="18" r="2" /><circle cx="19" cy="18" r="2" /><path d="m11 7-5 9m7-9 5 9M7 18h10" /></>,
  activity: <path d="M3 12h4l3-7 4 14 3-7h4" />,
  plus: <path d="M12 5v14M5 12h14" />,
  arrow: <path d="M4 12h15m-6-6 6 6-6 6" />,
  back: <path d="M20 12H5m6-6-6 6 6 6" />,
  eye: <><path d="M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6-10-6-10-6Z" /><circle cx="12" cy="12" r="2.5" /></>,
  eyeOff: <><path d="m3 3 18 18M10 6.2c.7-.1 1.3-.2 2-.2 6.5 0 10 6 10 6a14 14 0 0 1-2.1 2.5M6.2 7.6A15 15 0 0 0 2 12s3.5 6 10 6c1.5 0 2.9-.3 4.1-.8" /></>,
  chevronDown: <path d="m6 9 6 6 6-6" />,
  edit: <><path d="M13.5 6.5l4 4M4 20l4.5-1 11-11a2.8 2.8 0 0 0-4-4l-11 11Z" /></>,
  trash: <><path d="M4 7h16M10 11v6m4-6v6M6 7l1 13h10l1-13M9 7V4h6v3" /></>,
  link: <><path d="M10 13.5a4 4 0 0 0 5.6 0l3-3a4 4 0 0 0-5.6-5.6l-1.7 1.7m2.7 3.9a4 4 0 0 0-5.6 0l-3 3a4 4 0 0 0 5.6 5.6l1.7-1.7" /></>,
  logout: <path d="M10 4H5v16h5m3-8h8m-4-4 4 4-4 4" />,
  more: <><circle cx="5" cy="12" r="1" /><circle cx="12" cy="12" r="1" /><circle cx="19" cy="12" r="1" /></>,
  chevron: <path d="m9 6 6 6-6 6" />,
  menu: <path d="M4 6h16M4 12h16M4 18h16" />,
  close: <path d="m6 6 12 12M18 6 6 18" />,
  help: <><circle cx="12" cy="12" r="9" /><path d="M9.1 9a3 3 0 1 1 4.5 2.6c-1.1.7-1.6 1.2-1.6 2.4m0 3h.01" /></>,
  settings: <><path d="M4 7h16M4 17h16" /><circle cx="9" cy="7" r="2" fill="var(--color-paper-raised)" /><circle cx="15" cy="17" r="2" fill="var(--color-paper-raised)" /></>,
  check: <path d="m5 12 4 4L19 6" />,
  search: <><circle cx="10.5" cy="10.5" r="5.5" /><path d="m15 15 4 4" /></>,
  inbox: <><path d="M4 13 7 5h10l3 8v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1Z" /><path d="M4 13h5l1 3h4l1-3h5" /></>,
  panel: <><rect x="3" y="4" width="18" height="16" rx="2" /><path d="M15 4v16" /></>,
  expand: <path d="M9 4H4v5m11-5h5v5M4 15v5h5m11-5v5h-5" />,
  attach: <path d="m8 13 5.5-5.5a2 2 0 0 1 2.8 2.8l-7 7a3.5 3.5 0 0 1-5-5l8-8a5 5 0 0 1 7 7l-8 8" />,
  send: <path d="M12 19V5m-6 6 6-6 6 6" />,
} satisfies Record<string, ReactNode>;

export type IconName = keyof typeof paths;

export function Icon({ name, className = "size-[18px]" }: { name: IconName; className?: string }) {
  return (
    <svg aria-hidden="true" focusable="false" viewBox="0 0 24 24" fill="none" stroke="currentColor"
      strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round" className={className}>
      {paths[name]}
    </svg>
  );
}
