import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

// ChatMarkdown renders an agent's markdown answer (GitHub-flavoured: tables, lists,
// code, bold, links) with chat-bubble-appropriate Tailwind styling. react-markdown
// builds a React element tree (no dangerouslySetInnerHTML), so it is XSS-safe by
// construction — agent output is untrusted and must never be injected as raw HTML.
export function ChatMarkdown({ children }: { children: string }) {
  return (
    <div className="space-y-2 text-xs leading-relaxed">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          p: ({ children }) => <p className="whitespace-pre-wrap break-words">{children}</p>,
          ul: ({ children }) => <ul className="list-disc space-y-1 pl-4">{children}</ul>,
          ol: ({ children }) => <ol className="list-decimal space-y-1 pl-4">{children}</ol>,
          li: ({ children }) => <li className="marker:text-muted-foreground">{children}</li>,
          strong: ({ children }) => <strong className="font-semibold">{children}</strong>,
          em: ({ children }) => <em className="italic">{children}</em>,
          a: ({ href, children }) => (
            <a
              href={href}
              target="_blank"
              rel="noreferrer"
              className="text-primary underline underline-offset-2"
            >
              {children}
            </a>
          ),
          code: ({ className, children }) => {
            // A fenced block carries a language- class; inline code does not.
            if (/language-/.test(className ?? "")) {
              return <code className="font-mono">{children}</code>;
            }
            return (
              <code className="rounded bg-surface-2 px-1 py-0.5 font-mono text-[11px]">
                {children}
              </code>
            );
          },
          pre: ({ children }) => (
            <pre className="overflow-x-auto rounded-md bg-surface-2 p-2 font-mono text-[11px]">
              {children}
            </pre>
          ),
          table: ({ children }) => (
            <div className="overflow-x-auto rounded-md border border-border">
              <table className="w-full border-collapse text-[11px]">{children}</table>
            </div>
          ),
          thead: ({ children }) => <thead className="bg-surface-2">{children}</thead>,
          th: ({ children }) => (
            <th className="px-2.5 py-1.5 text-left font-semibold">{children}</th>
          ),
          td: ({ children }) => (
            <td className="border-t border-border/60 px-2.5 py-1.5 align-top">{children}</td>
          ),
          h1: ({ children }) => <h1 className="text-sm font-semibold">{children}</h1>,
          h2: ({ children }) => <h2 className="text-sm font-semibold">{children}</h2>,
          h3: ({ children }) => <h3 className="text-xs font-semibold">{children}</h3>,
          hr: () => <hr className="border-border/60" />,
          blockquote: ({ children }) => (
            <blockquote className="border-l-2 border-border pl-3 text-muted-foreground">
              {children}
            </blockquote>
          ),
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
