// <Icon name="bell" /> draws a lucide icon by its kebab-case name. Decorative (aria-hidden): pair it with text
// or an aria-label. Icons are imported by name so esbuild drops every icon that is not listed here.
// To use a new one: import it below and add it to ICONS. You can also pass the component: <Icon icon={Bell} />.
import {
  Activity, ArrowUpDown, Ban, Bell, BellOff, BellRing, Brain, Check, ChevronDown, ChevronLeft, ChevronRight, ChevronUp,
  ChartLine, Circle, CircleAlert, CircleCheck, CircleDot, CircleMinus, CircleX, Clock, Copy, Cpu, Database, Download,
  Eraser, ExternalLink, Eye, Filter, Flame, Gauge, HardDrive, History, Info, Layers, LayoutDashboard, ListChecks, Loader,
  Lock, LogOut, MemoryStick, Minus, MonitorSmartphone, Moon, OctagonAlert, PanelLeftClose, PanelLeftOpen, Pause, Play, Plus, RefreshCw, Rocket,
  RotateCcw, Save, ScrollText, Search, Send, Server, Settings, Share, Shield, ShieldAlert, ShieldCheck, SlidersHorizontal,
  Smartphone, Square, SquarePlus, Sun, Table2, Timer, Trash2, TriangleAlert, Undo2, User, WifiOff, X, Zap,
} from 'lucide-react';

export const ICONS = {
  activity: Activity, 'arrow-up-down': ArrowUpDown, ban: Ban, bell: Bell, 'bell-off': BellOff, 'bell-ring': BellRing,
  brain: Brain, check: Check, 'chevron-down': ChevronDown, 'chevron-left': ChevronLeft, 'chevron-right': ChevronRight,
  'chevron-up': ChevronUp, 'chart-line': ChartLine, circle: Circle, 'circle-alert': CircleAlert, 'circle-check': CircleCheck,
  'circle-dot': CircleDot, 'circle-minus': CircleMinus, 'circle-x': CircleX, clock: Clock, copy: Copy, cpu: Cpu,
  database: Database, download: Download, eraser: Eraser, 'external-link': ExternalLink, eye: Eye, filter: Filter,
  flame: Flame, gauge: Gauge, 'hard-drive': HardDrive, history: History, info: Info, layers: Layers,
  'layout-dashboard': LayoutDashboard, 'list-checks': ListChecks, loader: Loader, lock: Lock, 'log-out': LogOut,
  'memory-stick': MemoryStick, minus: Minus, 'monitor-smartphone': MonitorSmartphone, moon: Moon,
  'octagon-alert': OctagonAlert, 'panel-left-close': PanelLeftClose, 'panel-left-open': PanelLeftOpen, pause: Pause, play: Play, plus: Plus, 'refresh-cw': RefreshCw, rocket: Rocket,
  'rotate-ccw': RotateCcw, save: Save, 'scroll-text': ScrollText, search: Search, send: Send, server: Server,
  settings: Settings, share: Share, shield: Shield, 'shield-alert': ShieldAlert, 'shield-check': ShieldCheck,
  'sliders-horizontal': SlidersHorizontal, smartphone: Smartphone, square: Square, 'square-plus': SquarePlus, sun: Sun,
  'table-2': Table2, timer: Timer, 'trash-2': Trash2, 'triangle-alert': TriangleAlert, 'undo-2': Undo2, user: User,
  'wifi-off': WifiOff, x: X, zap: Zap,
};

export function Icon({ name, icon, size = 18, class: cls, className, strokeWidth = 2 }) {
  const Cmp = icon || ICONS[name] || Circle;
  const c = [cls, className].filter(Boolean).join(' ');
  return <Cmp className={c ? 'icon ' + c : 'icon'} size={size} strokeWidth={strokeWidth} aria-hidden="true" focusable="false" />;
}

export default Icon;
