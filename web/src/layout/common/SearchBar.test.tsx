import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { mocked } from 'jest-mock';

import API from '../../api';
import { SearchResults } from '../../types';
import { prepareQueryString } from '../../utils/prepareQueryString';
import SearchBar from './SearchBar';
jest.mock('../../api');

const mockHistoryPush = jest.fn();

jest.mock('react-router-dom', () => ({
  ...(jest.requireActual('react-router-dom') as {}),
  useHistory: () => ({
    push: mockHistoryPush,
  }),
}));

const getMockSearch = (fixtureId: string): SearchResults => {
  return require(`./__fixtures__/SearchBar/${fixtureId}.json`) as SearchResults;
};

const defaultProps = {
  tsQueryWeb: 'test',
  size: 'big' as 'big' | 'normal',
  openTips: false,
  setOpenTips: jest.fn(),
};

describe('SearchBar', () => {
  afterEach(() => {
    jest.resetAllMocks();
  });

  it('creates snapshot', () => {
    const { asFragment } = render(<SearchBar {...defaultProps} isSearching={false} />);
    expect(asFragment()).toMatchSnapshot();
  });

  it('renders loading when is searching', () => {
    render(<SearchBar {...defaultProps} isSearching />);
    expect(screen.getByTestId('searchBarSpinning')).toBeInTheDocument();
  });

  it('renders loading when is searching', () => {
    render(<SearchBar {...defaultProps} isSearching />);

    const spinning = screen.getByTestId('searchBarSpinning');
    const input = screen.getByPlaceholderText('Search packages') as HTMLInputElement;

    expect(spinning).toBeInTheDocument();
    expect(input.disabled).toBeTruthy();
  });

  it('focuses input when clean button is clicked', () => {
    render(<SearchBar {...defaultProps} isSearching={false} />);

    const cleanBtn = screen.getByRole('button', { name: 'Close' });
    const input = screen.getByPlaceholderText('Search packages') as HTMLInputElement;

    expect(input.value).toBe('test');
    userEvent.click(cleanBtn);
    expect(input).toBe(document.activeElement);
    expect(input.value).toBe('');
  });

  it('updates value on change input', async () => {
    render(<SearchBar {...defaultProps} isSearching={false} />);

    const input = screen.getByPlaceholderText('Search packages') as HTMLInputElement;

    expect(input.value).toBe('test');
    userEvent.type(input, 'ing');
    expect(input.value).toBe('testing');

    await waitFor(() => {
      expect(API.searchPackages).toHaveBeenCalledTimes(1);
    });
  });

  describe('search packages', () => {
    it('display search results', async () => {
      const mockSearch = getMockSearch('1');
      mocked(API).searchPackages.mockResolvedValue(mockSearch);

      render(<SearchBar {...defaultProps} isSearching={false} />);

      const input = screen.getByPlaceholderText('Search packages') as HTMLInputElement;

      expect(input.value).toBe('test');
      input.focus();
      userEvent.type(input, 'ing');

      await waitFor(() => {
        expect(API.searchPackages).toHaveBeenCalledTimes(1);
      });

      expect(screen.getByRole('listbox')).toBeInTheDocument();
      expect(screen.getAllByRole('option')).toHaveLength(3);
    });

    it("doesn't display results when input is not focused", async () => {
      const mockSearch = getMockSearch('2');
      mocked(API).searchPackages.mockResolvedValue(mockSearch);

      render(<SearchBar {...defaultProps} isSearching={false} />);

      const input = screen.getByPlaceholderText('Search packages') as HTMLInputElement;

      expect(input.value).toBe('test');
      input.focus();
      userEvent.type(input, 'ing');
      input.blur();

      await waitFor(() => {
        expect(API.searchPackages).toHaveBeenCalledTimes(1);
        input.blur();
      });

      expect(screen.queryByRole('listbox')).toBeNull();
    });
  });

  describe('History push', () => {
    it('calls on Enter key press', () => {
      render(<SearchBar {...defaultProps} isSearching={false} />);

      const input = screen.getByPlaceholderText('Search packages') as HTMLInputElement;
      userEvent.type(input, 'ing{enter}');
      expect(input).not.toBe(document.activeElement);
      expect(mockHistoryPush).toHaveBeenCalledTimes(1);
      expect(mockHistoryPush).toHaveBeenCalledWith({
        pathname: '/packages/search',
        search: prepareQueryString({
          tsQueryWeb: 'testing',
          pageNumber: 1,
        }),
      });
    });

    it('calls history push on Enter key press when text is empty with undefined text', () => {
      render(<SearchBar {...defaultProps} tsQueryWeb={undefined} isSearching={false} />);

      const input = screen.getByPlaceholderText('Search packages') as HTMLInputElement;
      userEvent.type(input, '{enter}');
      expect(mockHistoryPush).toHaveBeenCalledTimes(1);
      expect(mockHistoryPush).toHaveBeenCalledWith({
        pathname: '/packages/search',
        search: prepareQueryString({
          tsQueryWeb: undefined,
          pageNumber: 1,
        }),
      });
    });

    it('forces focus to click search bar icon', () => {
      render(<SearchBar {...defaultProps} tsQueryWeb={undefined} isSearching={false} />);

      const icon = screen.getByTestId('searchBarIcon');
      userEvent.click(icon);

      expect(screen.getByPlaceholderText('Search packages')).toHaveFocus();
    });
  });
});
