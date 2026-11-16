interface Debuggee { tabId?: number }
interface ChromeStorageArea {
  get(keys: string[]): Promise<Record<string, unknown>>;
  set(items: Record<string, unknown>): Promise<void>;
}
interface ChromeEvent<T extends (...args: never[]) => unknown> {
  addListener(listener: T): void;
}
declare const chrome: {
  storage: { local: ChromeStorageArea; session: ChromeStorageArea };
  tabs: { query(query: { active: boolean; currentWindow: boolean }): Promise<Array<{ id?: number }>> };
  debugger: {
    attach(target: Debuggee, version: string): Promise<void>;
    detach(target: Debuggee): Promise<void>;
    sendCommand(target: Debuggee, method: string, params?: object): Promise<unknown>;
    onEvent: ChromeEvent<(source: Debuggee, method: string, params?: unknown) => void>;
    onDetach: ChromeEvent<(source: Debuggee) => void>;
  };
  action: {
    setBadgeText(details: { text: string }): Promise<void>;
    setBadgeBackgroundColor(details: { color: string }): Promise<void>;
  };
  runtime: {
    sendMessage(message: { type: string }): Promise<unknown>;
    onMessage: ChromeEvent<(message: unknown, sender: unknown, respond: (response: unknown) => void) => boolean>;
  };
};
