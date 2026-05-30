import { Moon, Sun } from "lucide-react";
import { useTheme } from "../../theme/ThemeProvider";

// ThemeToggle is a compact icon button that flips between light and dark.
export function ThemeToggle() {
  const { theme, toggleTheme } = useTheme();
  const isDark = theme === "dark";

  return (
    <button
      type="button"
      onClick={toggleTheme}
      title={isDark ? "Switch to light" : "Switch to dark"}
      aria-label="Toggle colour theme"
      className="flex h-9 w-9 items-center justify-center border border-border
        text-muted transition-colors hover:text-fg"
    >
      {isDark ? <Sun size={16} /> : <Moon size={16} />}
    </button>
  );
}
