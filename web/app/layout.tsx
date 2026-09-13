import type { Metadata, Viewport } from "next";
import { Bricolage_Grotesque, Inter_Tight, Geist_Mono } from "next/font/google";
import "./globals.css";
import { Grain } from "@/components/ui/grain";
import { AppShell } from "@/components/shell/app-shell";
import Providers from "./providers";

/**
 * The three house families, loaded the way ApexTick loads them and under the
 * same CSS variable names — those exact names are what the `@theme inline`
 * block in globals.css reads. The `.variable` classes MUST be on <html>: miss
 * that and there is no error anywhere, every family silently falls back to
 * system-ui, and the whole product quietly stops looking like itself.
 */
const display = Bricolage_Grotesque({
  subsets: ["latin"],
  variable: "--font-bricolage",
  display: "swap",
});

const body = Inter_Tight({
  subsets: ["latin"],
  variable: "--font-inter-tight",
  display: "swap",
});

const mono = Geist_Mono({
  subsets: ["latin"],
  variable: "--font-geist-mono",
  display: "swap",
});

export const metadata: Metadata = {
  title: { default: "RunMesh", template: "%s, RunMesh" },
  description: "The execution console for the RunMesh DAG job runtime.",
  // An operator console with a job id in every URL. There is nothing here a
  // search engine should hold, and a crawler following /jobs/{id} would be
  // polling the runtime on someone else's behalf.
  robots: { index: false, follow: false, nocache: true },
};

export const viewport: Viewport = {
  themeColor: "#0b0b0c",
  colorScheme: "dark",
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html
      lang="en"
      className={`${display.variable} ${body.variable} ${mono.variable} h-full antialiased`}
    >
      <body className="relative min-h-full bg-ink text-bone">
        {/*
          Parked above the viewport and slid in on focus, never display:none,
          so it stays in the tab order. Its target is the <main id="main">
          inside AppShell; if that ever moves, this link silently goes nowhere.
        */}
        <a
          href="#main"
          className="skip-link rounded-full bg-accent px-5 py-2.5 text-sm font-medium text-accent-ink"
        >
          Skip to content
        </a>
        <Grain />
        <Providers>
          <AppShell>{children}</AppShell>
        </Providers>
      </body>
    </html>
  );
}
