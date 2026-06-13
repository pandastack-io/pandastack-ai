import type { BaseLayoutProps } from "fumadocs-ui/layouts/shared";

export const baseOptions: BaseLayoutProps = {
  nav: {
    title: (
      <span className="flex items-center gap-1.5 font-semibold tracking-tight">
        <img
          src="/logo.svg"
          alt=""
          aria-hidden
          className="size-[18px] rounded-[4px]"
        />
        PandaStack
      </span>
    ),
    url: "/",
  },
  links: [
    { text: "Docs", url: "/docs" },
    { text: "Discord", url: "https://discord.gg/C7Du7XbG", external: true },
    { text: "Dashboard", url: "https://app.pandastack.ai", external: true },
    { text: "GitHub", url: "https://github.com/pandastack-ai/pandastack", external: true },
  ],
};
